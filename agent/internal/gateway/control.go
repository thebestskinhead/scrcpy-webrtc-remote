package gateway

import (
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf8"

	"scrcpy-webrtc-remote/pkg/types"
)

// ControlMessage builders (Client -> Device)
type ControlMessage struct{}

const (
	msgInjectKeycode            byte = 0x00
	msgInjectText               byte = 0x01
	msgInjectTouchEvent         byte = 0x02
	msgInjectScrollEvent        byte = 0x03
	msgBackOrScreenOn           byte = 0x04
	msgExpandNotificationPanel  byte = 0x05
	msgExpandSettingsPanel      byte = 0x06
	msgCollapsePanels           byte = 0x07
	msgGetClipboard             byte = 0x08
	msgSetClipboard             byte = 0x09
	msgSetDisplayPower          byte = 0x0A
	msgRotateDevice             byte = 0x0B
	msgUhidCreate               byte = 0x0C
	msgUhidInput                byte = 0x0D
	msgUhidDestroy              byte = 0x0E
	msgOpenHardKeyboardSettings byte = 0x0F
	msgStartApp                 byte = 0x10
	msgResetVideo               byte = 0x11
	msgCameraSetTorch           byte = 0x12
	msgCameraZoomIn             byte = 0x13
	msgCameraZoomOut            byte = 0x14
	msgResizeDisplay            byte = 0x15
	msgStartAudio               byte = 0x16
	msgRequestKeyframe          byte = 0x17
	msgChangeBitrate            byte = 0x18
)

const (
	PointerIDMouse         uint64 = 0xFFFFFFFFFFFFFFFF
	PointerIDGenericFinger uint64 = 0xFFFFFFFFFFFFFFFE
	PointerIDVirtualFinger uint64 = 0xFFFFFFFFFFFFFFFD
)

func u16fp(pressure float64) uint16 {
	if pressure <= 0 {
		return 0
	}
	if pressure >= 1.0 {
		return 0xFFFF
	}
	return uint16(pressure * 65535.0)
}

func i16Scroll(scroll float64) int16 {
	v := int(scroll * 2048.0)
	if v > 32767 {
		v = 32767
	}
	if v < -32768 {
		v = -32768
	}
	return int16(v)
}

func BuildInjectKeycode(action, keycode, repeat, metastate uint32) []byte {
	b := make([]byte, 14)
	b[0] = msgInjectKeycode
	b[1] = byte(action)
	binary.BigEndian.PutUint32(b[2:6], keycode)
	binary.BigEndian.PutUint32(b[6:10], repeat)
	binary.BigEndian.PutUint32(b[10:14], metastate)
	return b
}

func BuildInjectText(text string) []byte {
	encoded := []byte(text)
	if len(encoded) > 300 {
		// Truncate at a rune boundary — cutting mid-rune would append
		// mojibake to the injected text.
		encoded = encoded[:300]
		for len(encoded) > 0 && !utf8.Valid(encoded) {
			encoded = encoded[:len(encoded)-1]
		}
	}
	b := make([]byte, 5+len(encoded))
	b[0] = msgInjectText
	binary.BigEndian.PutUint32(b[1:5], uint32(len(encoded)))
	copy(b[5:], encoded)
	return b
}

func BuildInjectTouch(action byte, pointerID uint64, x, y uint32, width, height uint16, pressure float64, actionButton, buttons uint32) []byte {
	b := make([]byte, 32)
	b[0] = msgInjectTouchEvent
	b[1] = action
	binary.BigEndian.PutUint64(b[2:10], pointerID)
	binary.BigEndian.PutUint32(b[10:14], x)
	binary.BigEndian.PutUint32(b[14:18], y)
	binary.BigEndian.PutUint16(b[18:20], width)
	binary.BigEndian.PutUint16(b[20:22], height)
	binary.BigEndian.PutUint16(b[22:24], u16fp(pressure))
	binary.BigEndian.PutUint32(b[24:28], actionButton)
	binary.BigEndian.PutUint32(b[28:32], buttons)
	return b
}

func BuildInjectScroll(x, y uint32, width, height uint16, hscroll, vscroll float64, buttons uint32) []byte {
	b := make([]byte, 21)
	b[0] = msgInjectScrollEvent
	binary.BigEndian.PutUint32(b[1:5], x)
	binary.BigEndian.PutUint32(b[5:9], y)
	binary.BigEndian.PutUint16(b[9:11], width)
	binary.BigEndian.PutUint16(b[11:13], height)
	binary.BigEndian.PutUint16(b[13:15], uint16(i16Scroll(hscroll)))
	binary.BigEndian.PutUint16(b[15:17], uint16(i16Scroll(vscroll)))
	binary.BigEndian.PutUint32(b[17:21], buttons)
	return b
}

func BuildBackOrScreenOn(action byte) []byte {
	return []byte{msgBackOrScreenOn, action}
}

func BuildExpandNotificationPanel() []byte  { return []byte{msgExpandNotificationPanel} }
func BuildExpandSettingsPanel() []byte     { return []byte{msgExpandSettingsPanel} }
func BuildCollapsePanels() []byte          { return []byte{msgCollapsePanels} }
func BuildRotateDevice() []byte            { return []byte{msgRotateDevice} }
func BuildResetVideo() []byte              { return []byte{msgResetVideo} }
func BuildOpenHardKeyboardSettings() []byte { return []byte{msgOpenHardKeyboardSettings} }
func BuildRequestKeyframe() []byte               { return []byte{msgRequestKeyframe} }
func BuildChangeBitrate(bitrate int) []byte {
	b := make([]byte, 5)
	b[0] = msgChangeBitrate
	binary.BigEndian.PutUint32(b[1:], uint32(bitrate))
	return b
}

func BuildGetClipboard(copyKey byte) []byte {
	return []byte{msgGetClipboard, copyKey}
}

func BuildSetClipboard(sequence uint64, paste bool, text string) []byte {
	encoded := []byte(text)
	if len(encoded) > 262130 {
		encoded = encoded[:262130]
	}
	p := byte(0)
	if paste {
		p = 1
	}
	b := make([]byte, 14+len(encoded))
	b[0] = msgSetClipboard
	binary.BigEndian.PutUint64(b[1:9], sequence)
	b[9] = p
	binary.BigEndian.PutUint32(b[10:14], uint32(len(encoded)))
	copy(b[14:], encoded)
	return b
}

func BuildSetDisplayPower(on bool) []byte {
	v := byte(0)
	if on {
		v = 1
	}
	return []byte{msgSetDisplayPower, v}
}

func BuildStartApp(name string) []byte {
	encoded := []byte(name)
	if len(encoded) > 255 {
		encoded = encoded[:255]
	}
	b := make([]byte, 2+len(encoded))
	b[0] = msgStartApp
	b[1] = byte(len(encoded))
	copy(b[2:], encoded)
	return b
}

func BuildUhidCreate(id uint16, vendor, product uint16, name string, reportDesc []byte) []byte {
	encoded := []byte(name)
	if len(encoded) > 255 {
		encoded = encoded[:255]
	}

	total := 1 + 2*3 + 1 + len(encoded) + 2 + len(reportDesc)
	b := make([]byte, total)
	pos := 0

	b[pos] = msgUhidCreate
	pos++
	binary.BigEndian.PutUint16(b[pos:], id)
	pos += 2
	binary.BigEndian.PutUint16(b[pos:], vendor)
	pos += 2
	binary.BigEndian.PutUint16(b[pos:], product)
	pos += 2
	b[pos] = byte(len(encoded))
	pos++
	copy(b[pos:], encoded)
	pos += len(encoded)
	binary.BigEndian.PutUint16(b[pos:], uint16(len(reportDesc)))
	pos += 2
	copy(b[pos:], reportDesc)

	return b
}

func BuildUhidInput(id uint16, data []byte) []byte {
	b := make([]byte, 4+len(data))
	b[0] = msgUhidInput
	binary.BigEndian.PutUint16(b[1:3], id)
	binary.BigEndian.PutUint16(b[3:5], uint16(len(data))) // 4.0: 2-byte length prefix
	copy(b[5:], data)
	return b
}

func BuildUhidDestroy(id uint16) []byte {
	b := make([]byte, 3)
	b[0] = msgUhidDestroy
	binary.BigEndian.PutUint16(b[1:3], id)
	return b
}

// BuildStartAudio tells the server to unpause the deferred software audio encoder.
func BuildStartAudio() []byte { return []byte{msgStartAudio} }

// scrcpy 4.0 camera control messages.

func BuildCameraSetTorch(on bool) []byte {
	v := byte(0)
	if on {
		v = 1
	}
	return []byte{msgCameraSetTorch, v}
}

func BuildCameraZoomIn() []byte  { return []byte{msgCameraZoomIn} }
func BuildCameraZoomOut() []byte { return []byte{msgCameraZoomOut} }

func BuildResizeDisplay(width, height uint16) []byte {
	b := make([]byte, 5)
	b[0] = msgResizeDisplay
	binary.BigEndian.PutUint16(b[1:3], width)
	binary.BigEndian.PutUint16(b[3:5], height)
	return b
}

// DeviceMessage parser (Device -> Client)

// DeviceMsg is an alias for the shared type in pkg/types.
type DeviceMsg = types.DeviceMsg

func ParseDeviceMessages(buf []byte) ([]DeviceMsg, []byte, error) {
	var msgs []DeviceMsg
	for len(buf) > 0 {
		if len(buf) < 1 {
			break
		}
		msgType := buf[0]
		var msgLen int

		switch msgType {
		case 0x00: // CLIPBOARD
			if len(buf) < 5 {
				return msgs, buf, nil
			}
			textLen := binary.BigEndian.Uint32(buf[1:5])
			msgLen = 5 + int(textLen)
		case 0x01: // ACK_CLIPBOARD
			msgLen = 9
		case 0x02: // UHID_OUTPUT
			if len(buf) < 5 {
				return msgs, buf, nil
			}
			dataLen := binary.BigEndian.Uint16(buf[3:5])
			msgLen = 5 + int(dataLen)
		default:
			// resync: drop one byte
			buf = buf[1:]
			continue
		}

		if len(buf) < msgLen {
			return msgs, buf, nil
		}

		msg, err := decodeDeviceMessage(buf[:msgLen])
		if err != nil {
			return msgs, buf, fmt.Errorf("decode device message: %w", err)
		}
		msgs = append(msgs, msg)
		buf = buf[msgLen:]
	}
	return msgs, buf, nil
}

func decodeDeviceMessage(data []byte) (DeviceMsg, error) {
	msgType := data[0]
	switch msgType {
	case 0x00:
		textLen := binary.BigEndian.Uint32(data[1:5])
		text := string(data[5 : 5+textLen])
		return DeviceMsg{Type: "clipboard", Text: text}, nil
	case 0x01:
		seq := binary.BigEndian.Uint64(data[1:9])
		return DeviceMsg{Type: "ack_clipboard", Sequence: seq}, nil
	case 0x02:
		devID := binary.BigEndian.Uint16(data[1:3])
		dataLen := binary.BigEndian.Uint16(data[3:5])
		payload := data[5 : 5+dataLen]
		return DeviceMsg{Type: "uhid_output", DevID: devID, DataHex: fmt.Sprintf("%x", payload), Raw: payload}, nil
	default:
		return DeviceMsg{}, fmt.Errorf("unknown device message type: %d", msgType)
	}
}

// uint64 pointer id helper for negative python values
func ParsePointerID(v int64) uint64 {
	if v < 0 {
		return uint64(v + math.MaxInt64 + 1 + math.MaxInt64 + 1)
	}
	return uint64(v)
}
