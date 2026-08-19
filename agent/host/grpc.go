package host

import (
	"context"
	"strconv"

	agentapi "scrcpy-webrtc-remote/api/gen"
	"scrcpy-webrtc-remote/agent"
	"scrcpy-webrtc-remote/pkg/config"
	"scrcpy-webrtc-remote/pkg/logger"
)

// GRPCServer implements agentapi.AgentServiceServer（13 RPC）。
type GRPCServer struct {
	agentapi.UnimplementedAgentServiceServer
	h *Host
}

// NewGRPCServer creates the gRPC service implementation.
func NewGRPCServer(h *Host) *GRPCServer {
	return &GRPCServer{h: h}
}

func (s *GRPCServer) requireInit() bool {
	return s.h.State() != stateUninitialized
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

func (s *GRPCServer) Init(_ context.Context, req *agentapi.InitRequest) (*agentapi.InitResponse, error) {
	return s.h.Init(req), nil
}

func (s *GRPCServer) Start(_ context.Context, _ *agentapi.Empty) (*agentapi.CommonResponse, error) {
	return s.h.Start(), nil
}

func (s *GRPCServer) Stop(_ context.Context, _ *agentapi.Empty) (*agentapi.CommonResponse, error) {
	return s.h.Stop(), nil
}

func (s *GRPCServer) ReloadConfig(_ context.Context, req *agentapi.ReloadConfigRequest) (*agentapi.CommonResponse, error) {
	return s.h.ReloadConfig(req.GetGlobalConfig()), nil
}

// ---------------------------------------------------------------------------
// 设备管理
// ---------------------------------------------------------------------------

func (s *GRPCServer) PrepareDevice(_ context.Context, req *agentapi.PrepareDeviceRequest) (*agentapi.CommonResponse, error) {
	if !s.requireInit() {
		return errResp("ERR_NOT_INIT", "not initialized"), nil
	}
	if req.GetDeviceSerial() == "" || req.GetInstanceId() == "" {
		return errResp("ERR_INVALID_ARG", "device_serial and instance_id are required"), nil
	}
	dm := s.h.DeviceManager()
	devCfg := config.ToDeviceConfig(req.GetDeviceConfig())
	cp := toConnectParams(req.GetConnectParams())
	if err := dm.Prepare(req.GetDeviceSerial(), req.GetInstanceId(), devCfg, cp); err != nil {
		logger.Error("prepare device failed", "serial", req.GetDeviceSerial(), "err", err)
		return errResp("ERR_INTERNAL", err.Error()), nil
	}
	return okResp(), nil
}

func (s *GRPCServer) ReleaseDevice(_ context.Context, req *agentapi.ReleaseDeviceRequest) (*agentapi.CommonResponse, error) {
	if !s.requireInit() {
		return errResp("ERR_NOT_INIT", "not initialized"), nil
	}
	if err := s.h.DeviceManager().Release(req.GetDeviceSerial(), req.GetReason()); err != nil {
		return errResp("ERR_DEVICE_NOT_FOUND", err.Error()), nil
	}
	return okResp(), nil
}

func (s *GRPCServer) ResetDevice(_ context.Context, req *agentapi.ResetDeviceRequest) (*agentapi.CommonResponse, error) {
	if !s.requireInit() {
		return errResp("ERR_NOT_INIT", "not initialized"), nil
	}
	if err := s.h.DeviceManager().Reset(req.GetDeviceSerial(), req.GetReason()); err != nil {
		return errResp("ERR_DEVICE_NOT_FOUND", err.Error()), nil
	}
	return okResp(), nil
}

func (s *GRPCServer) ListDevices(_ context.Context, _ *agentapi.Empty) (*agentapi.ListDevicesResponse, error) {
	if !s.requireInit() {
		return &agentapi.ListDevicesResponse{Ok: false, ErrorCode: "ERR_NOT_INIT", Message: "not initialized"}, nil
	}
	views := s.h.DeviceManager().List()
	devices := make([]*agentapi.DeviceStatus, 0, len(views))
	for _, v := range views {
		devices = append(devices, &agentapi.DeviceStatus{
			InstanceId:         v.InstanceID,
			DeviceSerial:       v.Serial,
			Busy:               v.Busy,
			CurrentSessionId:   v.CurrentSessionID,
			ConnectedSeconds:   v.ConnectedSeconds,
			SignalingConnected: v.SignalingConnected,
			Configured:         v.Configured,
		})
	}
	return &agentapi.ListDevicesResponse{
		Ok: true, ErrorCode: "OK", Devices: devices,
	}, nil
}

// ---------------------------------------------------------------------------
// 会话干预（绝对管理权）
// ---------------------------------------------------------------------------

func (s *GRPCServer) ForceCloseSession(_ context.Context, req *agentapi.ForceCloseSessionRequest) (*agentapi.CommonResponse, error) {
	if !s.requireInit() {
		return errResp("ERR_NOT_INIT", "not initialized"), nil
	}
	if req.GetSessionId() == "" {
		return errResp("ERR_INVALID_ARG", "session_id is required"), nil
	}
	ctrl := s.h.DeviceManager().GetBySessionID(req.GetSessionId())
	if ctrl == nil {
		return errResp("ERR_SESSION_NOT_FOUND", "session not found"), nil
	}
	if err := ctrl.ForceCloseSession(req.GetSessionId(), req.GetReason()); err != nil {
		return errResp("ERR_SESSION_NOT_FOUND", err.Error()), nil
	}
	return okResp(), nil
}

func (s *GRPCServer) SetQuality(_ context.Context, req *agentapi.SetQualityRequest) (*agentapi.CommonResponse, error) {
	if !s.requireInit() {
		return errResp("ERR_NOT_INIT", "not initialized"), nil
	}
	ctrl := s.h.DeviceManager().Get(req.GetDeviceSerial())
	if ctrl == nil {
		return errResp("ERR_DEVICE_NOT_FOUND", "device not found"), nil
	}
	if err := ctrl.SetQuality(req.GetLevel()); err != nil {
		return errResp("ERR_INVALID_ARG", err.Error()), nil
	}
	return okResp(), nil
}

func (s *GRPCServer) SendControl(_ context.Context, req *agentapi.SendControlRequest) (*agentapi.CommonResponse, error) {
	if !s.requireInit() {
		return errResp("ERR_NOT_INIT", "not initialized"), nil
	}
	ctrl := s.h.DeviceManager().Get(req.GetDeviceSerial())
	if ctrl == nil {
		return errResp("ERR_DEVICE_NOT_FOUND", "device not found"), nil
	}
	ctl := req.GetControl()
	if ctl == nil || ctl.GetType() == "" {
		return errResp("ERR_INVALID_ARG", "control is required"), nil
	}
	if err := ctrl.SendControlRaw(ctl.GetType(), toControlData(ctl.GetData())); err != nil {
		return errResp("ERR_INVALID_ARG", err.Error()), nil
	}
	return okResp(), nil
}

// ---------------------------------------------------------------------------
// 探活 / 事件流
// ---------------------------------------------------------------------------

func (s *GRPCServer) Health(_ context.Context, _ *agentapi.Empty) (*agentapi.HealthResponse, error) {
	return s.h.Health(), nil
}

// StreamEvents 服务端流：订阅 eventHub，fan-out 推送；断线即取消订阅，
// 不缓冲不重放（平台以 ListDevices 补偿）。
func (s *GRPCServer) StreamEvents(_ *agentapi.Empty, stream agentapi.AgentService_StreamEventsServer) error {
	ch, cancel := s.h.EventHub().Subscribe()
	defer cancel()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func toConnectParams(p *agentapi.ConnectParams) agent.ConnectParams {
	if p == nil {
		return agent.ConnectParams{}
	}
	return agent.ConnectParams{
		ServiceID:      p.GetServiceId(),
		WSHeaders:      p.GetWsHeaders(),
		RegisterFields: p.GetRegisterFields(),
		WSPath:         p.GetWsPath(),
	}
}

// toControlData converts the proto map<string,string> into the numeric-friendly
// map[string]any the control dispatcher expects (数字字符串 → float64）。
func toControlData(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			out[k] = f
		} else {
			out[k] = v
		}
	}
	return out
}
