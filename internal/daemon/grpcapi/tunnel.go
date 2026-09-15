package grpcapi

import (
	"context"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
)

// ApplyTunnel 应用（新增/更新）一条穿透隧道：生成 frpc 配置并拉起 frpc。
func (s *Server) ApplyTunnel(ctx context.Context, req *pb.TunnelRequest) (*pb.OperationResponse, error) {
	if req.InstanceId == "" || req.TunnelId == "" {
		return &pb.OperationResponse{Success: false, Error: "instance_id 与 tunnel_id 必填"}, nil
	}
	if req.FrpsHost == "" || req.FrpsPort <= 0 {
		return &pb.OperationResponse{Success: false, Error: "frps 服务端信息不完整"}, nil
	}

	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}

	name := req.Name
	if name == "" {
		name = req.InstanceId + "-" + req.TunnelId
	}

	err := s.frp.Apply(req.InstanceId, inst.Dir, frp.Server{
		Host:     req.FrpsHost,
		BindPort: int(req.FrpsPort),
		Token:    req.FrpsToken,
	}, frp.Tunnel{
		TunnelID:   req.TunnelId,
		Name:       name,
		Protocol:   req.Protocol,
		LocalPort:  int(req.LocalPort),
		RemotePort: int(req.RemotePort),
	})
	if err != nil {
		s.log.Error("应用隧道失败", "instance", req.InstanceId, "tunnel", req.TunnelId, "error", err)
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}

	s.log.Info("隧道已应用", "instance", req.InstanceId, "tunnel", req.TunnelId,
		"remote_port", req.RemotePort, "local_port", req.LocalPort,
		"instance_running", inst.Status() == "running")

	// 实例没在跑时 frpc 不会启动（见 frp.Manager.SetInstanceState），
	// 所以"隧道已建立"会是一句假话 —— 如实说清楚，免得管理员以为公网已经通了。
	if inst.Status() != "running" {
		return &pb.OperationResponse{
			Success: true,
			Message: "隧道已登记（实例未运行，启动实例后自动生效）",
		}, nil
	}
	return &pb.OperationResponse{Success: true, Message: "隧道已建立"}, nil
}

// RemoveTunnel 移除一条隧道。
func (s *Server) RemoveTunnel(ctx context.Context, req *pb.TunnelRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	if err := s.frp.Remove(req.InstanceId, inst.Dir, req.TunnelId); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	s.log.Info("隧道已移除", "instance", req.InstanceId, "tunnel", req.TunnelId)
	return &pb.OperationResponse{Success: true, Message: "隧道已移除"}, nil
}

// ListTunnels 列出实例的隧道状态。
func (s *Server) ListTunnels(ctx context.Context, req *pb.ListTunnelsRequest) (*pb.ListTunnelsResponse, error) {
	statuses := s.frp.List(req.InstanceId)
	out := make([]*pb.TunnelInfo, 0, len(statuses))
	for _, st := range statuses {
		out = append(out, &pb.TunnelInfo{
			TunnelId:   st.TunnelID,
			Protocol:   st.Protocol,
			LocalPort:  int32(st.LocalPort),
			RemotePort: int32(st.RemotePort),
			Status:     st.Status,
			Error:      st.Error,
		})
	}
	return &pb.ListTunnelsResponse{Tunnels: out}, nil
}
