// Package debughooks defines the optional debug extension interface for the
// agent. A nil Hooks (the default) disables all debug functionality in
// production builds: the debug protocol handlers, metric sampling and the
// netem weak-network simulator are only compiled into closed-source builds
// that inject a Hooks implementation.
//
// The interface deliberately exposes no privileged operations. Collecting,
// persisting and serving debug logs (debug_start/debug_stop/debug_fetch) is
// implemented by the injected Hooks; the open-source code only forwards
// control messages and formatted log lines to it.
package debughooks

import (
	"time"

	"github.com/pion/interceptor"
)

// SessionMetrics is a snapshot of internal session state handed to the hooks
// for periodic sampling. The open-source controller collects these values
// (plain reads of internal counters) and the closed-source Hooks decide what
// to record and when (throttling, formatting, persistence).
type SessionMetrics struct {
	EventQLen    int    // pending events in the session queue
	VideoChLen   int    // buffered video frames
	CtrlChLen    int    // buffered control messages
	SessionState string // scrcpy session state
	Paused       bool   // forwarding paused
	PeerState    string // WebRTC peer state
	Bitrate      int    // current encoder bitrate
}

// Hooks is the debug extension point injected into an agent Controller.
// nil disables everything.
type Hooks interface {
	// Logf records a formatted debug line when collection is active.
	Logf(format string, args ...any)

	// Active reports whether debug collection is currently running.
	Active() bool

	// Remaining returns the time left before collection auto-stops
	// (0 = unlimited / not collecting).
	Remaining() time.Duration

	// Start begins collection for the given duration (<=0 = until Stop).
	Start(duration time.Duration) error

	// Stop ends collection and persists the buffer to a file.
	// Returns (file path, line count, error).
	Stop() (string, int, error)

	// Snapshot returns the collected log lines.
	Snapshot() []string

	// BindPeer hands the hooks a callback that sends a raw payload over the
	// browser DataChannel (used by the debug protocol to push collected logs
	// to the frontend). Called by the controller once the peer is created;
	// may be nil if no peer is available.
	BindPeer(send func(payload []byte) error)

	// HandleControl processes a control-channel message the open-source
	// switch did not recognize (debug_start / debug_stop / debug_fetch …).
	// Returns true when the message was consumed.
	HandleControl(msgType string, ctrl map[string]any) bool

	// OnSessionEvent is invoked by the session event loop with a metrics
	// snapshot. Throttling is the hooks' responsibility.
	OnSessionEvent(m SessionMetrics)

	// OnControlSent is invoked whenever a control message is forwarded to
	// scrcpy. Throttling/aggregation is the hooks' responsibility.
	OnControlSent(b []byte)

	// CreateInterceptors returns extra interceptor factories to append to the
	// peer's interceptor chain (e.g. the netem weak-network simulator).
	// Factories are registered socket-side, before the NACK responder, so
	// dropped/delayed packets are still buffered and retransmittable.
	CreateInterceptors() []interceptor.Factory
}
