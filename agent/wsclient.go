package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"scrcpy-webrtc-remote/pkg/common"
)

// Client is a WebSocket connection to the signaling server from the agent side.
type Client struct {
	conn    *websocket.Conn
	writeMu sync.Mutex // guards WriteJSON; goroutine A (main loop) and goroutine B (SessionCtx)
	// both write to the same conn, and WebSocket frames must not interleave.
}

// Connect dials the signaling server, sends a register message, and returns
// a ready-to-use Client.
func Connect(url, serviceID, instanceID, deviceSerial string) (*Client, error) {
	ws, _, err := websocket.DefaultDialer.Dial(url+"/ws/agent/"+serviceID, nil)
	if err != nil {
		return nil, fmt.Errorf("dial signaling: %w", err)
	}

	// First message: register
	if err := ws.WriteJSON(common.WsMsg{
		Type:         common.TypeRegister,
		ServiceID:    serviceID,
		InstanceID:   instanceID,
		DeviceSerial: deviceSerial,
	}); err != nil {
		ws.Close()
		return nil, fmt.Errorf("send register: %w", err)
	}

	// Wait for registered confirmation
	var ack common.WsMsg
	if err := ws.ReadJSON(&ack); err != nil {
		ws.Close()
		return nil, fmt.Errorf("read registered ack: %w", err)
	}
	if ack.Type != "registered" {
		ws.Close()
		return nil, fmt.Errorf("unexpected ack: %s", ack.Type)
	}

	return &Client{conn: ws}, nil
}

// Read reads the next message from signaling.
func (c *Client) Read() (common.WsMsg, error) {
	var msg common.WsMsg
	err := c.conn.ReadJSON(&msg)
	return msg, err
}

// Send writes a message to signaling. goroutine-safe.
func (c *Client) Send(msg common.WsMsg) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteJSON(msg)
}

// Heartbeat starts a background goroutine that sends "ping" messages at the
// given interval.  The goroutine exits when ctx is canceled.
// This keeps the WebSocket alive through proxies / load balancers that drop
// idle connections.
func (c *Client) Heartbeat(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = c.Send(common.WsMsg{Type: common.TypePing})
			}
		}
	}()
}

// Close closes the WebSocket.
func (c *Client) Close() error {
	return c.conn.Close()
}
