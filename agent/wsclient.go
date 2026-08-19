package agent

import (
	"context"
	"fmt"
	"net/http"
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

// ConnectParams 覆盖设备信令连接参数（来自 PrepareDevice.connect_params）。
// 未设置字段回退全局配置（service_id）或默认路径。
type ConnectParams struct {
	ServiceID      string            // 非空则覆盖全局 ServiceID
	WSHeaders      map[string]string // 附加 WS 握手头
	RegisterFields map[string]string // 注册消息附加字段（并入 register.Data）
	WSPath         string            // 覆盖默认 /ws/agent/<service>
}

// Connect dials the signaling server with default params (legacy entry).
func Connect(url, serviceID, instanceID, deviceSerial string) (*Client, error) {
	return ConnectWithParams(url, serviceID, instanceID, deviceSerial, ConnectParams{})
}

// ConnectWithParams dials the signaling server, sends a register message, and
// returns a ready-to-use Client. p 的覆盖规则见 ConnectParams。
func ConnectWithParams(url, serviceID, instanceID, deviceSerial string, p ConnectParams) (*Client, error) {
	svc := serviceID
	if p.ServiceID != "" {
		svc = p.ServiceID
	}
	path := "/ws/agent/" + svc
	if p.WSPath != "" {
		path = p.WSPath
	}

	dialer := websocket.DefaultDialer
	var header http.Header
	if len(p.WSHeaders) > 0 {
		header = make(http.Header, len(p.WSHeaders))
		for k, v := range p.WSHeaders {
			header.Set(k, v)
		}
	}
	ws, _, err := dialer.Dial(url+path, header)
	if err != nil {
		return nil, fmt.Errorf("dial signaling: %w", err)
	}

	// First message: register
	reg := common.WsMsg{
		Type:         common.TypeRegister,
		ServiceID:    svc,
		InstanceID:   instanceID,
		DeviceSerial: deviceSerial,
	}
	if len(p.RegisterFields) > 0 {
		reg.Data = make(map[string]any, len(p.RegisterFields))
		for k, v := range p.RegisterFields {
			reg.Data[k] = v
		}
	}
	if err := ws.WriteJSON(reg); err != nil {
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
