package gateway

import (
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"time"
)

// scrcpy 4.0 packet header flags:
//   bit 63: session meta packet
//   bit 62: config packet (codec configuration data)
//   bit 61: key frame
//   bits 0-60: PTS (presentation timestamp in microseconds)
const (
	PacketFlagSession  uint64 = 1 << 63
	PacketFlagConfig   uint64 = 1 << 62
	PacketFlagKeyFrame uint64 = 1 << 61
	PacketPTSMask      uint64 = PacketFlagKeyFrame - 1
)

// DisableStreamCode is the codec ID used by the server to signal that a stream
// is disabled (non-fatal) or has a configuration error (fatal).
const (
	DisableStreamCode       uint32 = 0x00_00_00_00 // stream explicitly disabled
	DisableStreamCodeError  uint32 = 0x00_00_00_01 // configuration error
)

type FrameHeader struct {
	PTSAndFlags uint64
	PacketSize  uint32
}

func (h FrameHeader) PTS() time.Duration {
	return time.Duration(h.PTSAndFlags&PacketPTSMask) * time.Microsecond
}

func (h FrameHeader) IsConfig() bool {
	return (h.PTSAndFlags & PacketFlagConfig) != 0
}

func (h FrameHeader) IsKeyFrame() bool {
	return (h.PTSAndFlags & PacketFlagKeyFrame) != 0
}

func (h FrameHeader) IsSessionMeta() bool {
	return (h.PTSAndFlags & PacketFlagSession) != 0
}

// SessionMeta represents a scrcpy 4.0 session metadata packet.
// These packets carry display size and are sent at stream start and on resize.
type SessionMeta struct {
	Width          uint32
	Height         uint32
	IsClientResize bool
}

type VideoCodecMeta struct {
	CodecID uint32
	Width   uint32
	Height  uint32
}

func (m VideoCodecMeta) String() string {
	return fmt.Sprintf("codec=%08x width=%d height=%d", m.CodecID, m.Width, m.Height)
}

type Reader struct {
	r io.Reader
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// ReadVideoCodecID reads the scrcpy 4.0 video stream header:
//   1 byte dummy + 64 bytes device name + 4 bytes codec ID.
// Unlike 2.1, width/height are delivered later via SessionMeta packets.
func (vr *Reader) ReadVideoCodecID() (uint32, error) {
	skip := make([]byte, 65) // 1 byte dummy + 64 bytes device name
	if _, err := io.ReadFull(vr.r, skip); err != nil {
		return 0, fmt.Errorf("read video handshake: %w", err)
	}

	buf := make([]byte, 4)
	if _, err := io.ReadFull(vr.r, buf); err != nil {
		return 0, fmt.Errorf("read video codec id: %w", err)
	}
	return binary.BigEndian.Uint32(buf), nil
}

// ReadAudioCodecID reads the 4-byte audio codec ID from the audio stream header.
// Returns an error if the server sends a disable-stream signal (0x00 or 0x01).
func (vr *Reader) ReadAudioCodecID() (uint32, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(vr.r, buf); err != nil {
		return 0, fmt.Errorf("read audio codec id: %w", err)
	}
	codecID := binary.BigEndian.Uint32(buf)
	if codecID == DisableStreamCode {
		return 0, fmt.Errorf("audio stream disabled by server")
	}
	if codecID == DisableStreamCodeError {
		return 0, fmt.Errorf("audio stream configuration error on server")
	}
	return codecID, nil
}

// ReadPacket reads one packet from the stream.
// The returned SessionMeta is non-nil when a scrcpy 4.0 session metadata packet
// (PACKET_FLAG_SESSION set) is encountered. In that case, payload will be nil.
func (vr *Reader) ReadPacket() (FrameHeader, []byte, *SessionMeta, error) {
	header := make([]byte, 12)
	if _, err := io.ReadFull(vr.r, header); err != nil {
		return FrameHeader{}, nil, nil, fmt.Errorf("read packet header: %w", err)
	}

	fh := FrameHeader{
		PTSAndFlags: binary.BigEndian.Uint64(header[0:8]),
		PacketSize:  binary.BigEndian.Uint32(header[8:12]),
	}

	// scrcpy 4.0: Session Meta packet uses PACKET_FLAG_SESSION (bit 63).
	// The 12-byte header contains:
	//   [0:4] flags (int32, bit31=session-flag, bit0=isClientResize)
	//   [4:8] width (uint32 BE)
	//   [8:12] height (uint32 BE)
	if fh.IsSessionMeta() {
		flags := binary.BigEndian.Uint32(header[0:4])
		return fh, nil, &SessionMeta{
			Width:          binary.BigEndian.Uint32(header[4:8]),
			Height:         binary.BigEndian.Uint32(header[8:12]),
			IsClientResize: (flags & 1) != 0,
		}, nil
	}

	if fh.PacketSize == 0 {
		return fh, nil, nil, fmt.Errorf("packet size is zero")
	}
	if fh.PacketSize > 50_000_000 {
		return fh, nil, nil, fmt.Errorf("packet size %d exceeds limit", fh.PacketSize)
	}

	payload := make([]byte, fh.PacketSize)
	if _, err := io.ReadFull(vr.r, payload); err != nil {
		return fh, nil, nil, fmt.Errorf("read packet payload: %w", err)
	}

	return fh, payload, nil, nil
}

// FindStartCode 安全定位 Annex-B Start Code
// 优先匹配 4-byte Start Code (00 00 00 01)
// 次匹配 3-byte Start Code (00 00 01)，排除属于 4-byte 后半截的情况（前一字节为 00 时跳过）
func FindStartCode(data []byte) (pos int, length int, found bool) {
	for i := 0; i < len(data)-3; i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			return i, 4, true
		}
	}
	for i := 0; i < len(data)-2; i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			// 排除属于 4-byte Start Code 后半截的情况
			if i > 0 && data[i-1] == 0 {
				continue
			}
			return i, 3, true
		}
	}
	return 0, 0, false
}

// FindSPSInAnnexB 从 Annex-B payload 中提取首个 SPS NAL Unit（不含 Start Code）
func FindSPSInAnnexB(payload []byte) (sps []byte, found bool) {
	pos, length, ok := FindStartCode(payload)
	if !ok {
		return nil, false
	}

	remaining := payload[pos+length:]

	for {
		nextPos, _, nextOk := FindStartCode(remaining)
		if !nextOk {
			// 没有下一个 Start Code，整个 remaining 就是最后一个 NAL
			if len(remaining) > 0 {
				nalType := remaining[0] & 0x1F
				if nalType == 7 {
					return remaining, true
				}
			}
			break
		}

		nal := remaining[:nextPos]
		if len(nal) > 0 {
			nalType := nal[0] & 0x1F
			if nalType == 7 {
				return nal, true
			}
		}

		// 跳到下一个 NAL
		remaining = remaining[nextPos:]
		// 跳过 Start Code
		if len(remaining) >= 3 && remaining[0] == 0 && remaining[1] == 0 && remaining[2] == 1 {
			if len(remaining) >= 4 && remaining[3] == 1 {
				remaining = remaining[4:]
			} else {
				remaining = remaining[3:]
			}
		}
	}

	return nil, false
}

// ExtractProfileLevelID 从 SPS NAL Unit（含 NAL header）中提取 profile-level-id
func ExtractProfileLevelID(sps []byte) (string, error) {
	if len(sps) < 4 {
		return "", fmt.Errorf("sps too short: %d bytes", len(sps))
	}
	// SPS 结构：NAL header(1 byte) + profile_idc(1) + constraint_set(1) + level_idc(1)
	profileIDC := sps[1]
	constraintSet := sps[2]
	levelIDC := sps[3]
	return fmt.Sprintf("%02x%02x%02x", profileIDC, constraintSet, levelIDC), nil
}

// NormalizeProfileLevelID 将设备真实 PLID 映射到浏览器 offer 中存在的值，
// 解决 pion 在 SetLocalDescription 时做的 fmtp 行精确匹配。
// SPS 字节不受影响——只有 SDP fmtp 行使用归一化后的值。
func NormalizeProfileLevelID(plid string) string {
	if len(plid) != 6 {
		return plid
	}
	// 解码 profile_idc（前 2 个 hex）
	profileIDC, _ := strconv.ParseUint(plid[0:2], 16, 8)
	// 保留原 level_idc（后 2 个 hex），不改
	levelStr := plid[4:6]

	switch profileIDC {
	case 66: // Baseline → Constrained Baseline (constraint_set=0xe0)
		return "42e0" + levelStr
	case 77: // Main → Main (constraint_set=0x00)
		return "4d00" + levelStr
	case 100: // High → High (constraint_set=0x00)
		return "6400" + levelStr
	case 244: // High 4:4:4
		return "f400" + levelStr
	default:
		// Unknown profile: keep constraint_set=0x00
		return plid[0:2] + "00" + levelStr
	}
}

// tryExtractProfileLevelID 尝试从多种格式中提取 profile-level-id。
// 依次尝试：AnnexB 起始码分隔、裸 NALU（首字节为 SPS header）、AVCC（4 字节长度前缀）。
func tryExtractProfileLevelID(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}

	// 1. AnnexB 格式（含 00 00 00 01 或 00 00 01）
	if sps, found := FindSPSInAnnexB(payload); found {
		if plid, err := ExtractProfileLevelID(sps); err == nil {
			return plid
		}
	}

	// 2. 裸 NALU：首字节就是 SPS NAL header（0x67 等）
	if payload[0]&0x1F == 7 {
		if plid, err := ExtractProfileLevelID(payload); err == nil {
			return plid
		}
	}

	// 3. AVCC 格式：4 字节大端长度前缀 + NALU
	if len(payload) >= 8 {
		spsLen := int(binary.BigEndian.Uint32(payload[0:4]))
		if spsLen > 0 && spsLen <= len(payload)-4 {
			sps := payload[4 : 4+spsLen]
			if len(sps) > 0 && sps[0]&0x1F == 7 {
				if plid, err := ExtractProfileLevelID(sps); err == nil {
					return plid
				}
			}
		}
	}

	return ""
}

