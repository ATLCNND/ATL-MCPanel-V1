package grpcapi

import (
	"context"
	"path/filepath"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/portguard"
	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ApplyTunnel 应用（新增/更新）一条穿透隧道：生成 frpc 配置并拉起 frpc。
func (s *Server) ApplyTunnel(ctx context.Context, req *pb.TunnelRequest) (*pb.OperationResponse, error) {
	if req.InstanceId == "" || req.TunnelId == "" {
		return &pb.OperationResponse{Success: false, Error: "instance_id 与 tunnel_id 必填"}, nil
	}
	if req.FrpsHost == "" || req.FrpsPort <= 0 {
		return &pb.OperationResponse{Success: false, Error: "frps 服务端信息不完整"}, nil
	}

	if _, ok := s.reg.Get(req.InstanceId); !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}

	// 本地端口安全检查（最后一道）。
	//
	// 面板已经拦过一次，但 Daemon 不能假设调用方一定是自家面板：这条 RPC 只要
	// 能连上就能调，而 frpc 是以 root 跑的、生成的配置固定
	// `localAddr = "127.0.0.1:<local_port>"` —— 放行 22 就等于把节点的 SSH
	// 挂到公网。这里用**本机实际监听**的端口再兜一道。
	if err := portguard.Check(int(req.LocalPort), s.protectedLocalPorts()); err != nil {
		s.log.Warn("拒绝穿透目标：本地端口指向平台自身服务",
			"instance", req.InstanceId, "local_port", req.LocalPort, "reason", err.Error())
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}

	name := req.Name
	if name == "" {
		name = req.InstanceId + "-" + req.TunnelId
	}

	err := s.frp.Apply(req.InstanceId, s.frpDir(req.InstanceId), frp.Server{
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

	// 实例状态用来决定回执措辞：实例没在跑时 frpc 不会启动
	//（见 frp.Manager.SetInstanceState），所以"隧道已建立"会是一句假话 ——
	// 如实说清楚，免得管理员以为公网已经通了。
	running := false
	if inst, ok := s.reg.Get(req.InstanceId); ok {
		running = inst.Status() == "running"
	}

	s.log.Info("隧道已应用", "instance", req.InstanceId, "tunnel", req.TunnelId,
		"remote_port", req.RemotePort, "local_port", req.LocalPort,
		"instance_running", running)

	if !running {
		return &pb.OperationResponse{
			Success: true,
			Message: "隧道已登记（实例未运行，启动实例后自动生效）",
		}, nil
	}
	return &pb.OperationResponse{Success: true, Message: "隧道已建立"}, nil
}

// RemoveTunnel 移除一条隧道。
func (s *Server) RemoveTunnel(ctx context.Context, req *pb.TunnelRequest) (*pb.OperationResponse, error) {
	if _, ok := s.reg.Get(req.InstanceId); !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	if err := s.frp.Remove(req.InstanceId, s.frpDir(req.InstanceId), req.TunnelId); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	s.log.Info("隧道已移除", "instance", req.InstanceId, "tunnel", req.TunnelId)
	return &pb.OperationResponse{Success: true, Message: "隧道已移除"}, nil
}

// protectedLocalPorts 本机平台自己占用的端口（穿透目标不许指向它们）。
//
// 默认清单只能覆盖默认端口，所以这里把**本机配置里的真实端口**也加进去：
// Daemon gRPC 监听、frps 管理接口地址、以及面板与本机同机时的面板端口。
func (s *Server) protectedLocalPorts() map[int]string {
	m := map[int]string{}
	why := "平台自身服务占用的端口（穿透目标指向它会把该服务暴露到公网）"
	if s.cfg != nil {
		portguard.Set(m, why, s.cfg.GRPCListen, s.cfg.FRPAdminAddr)
	}
	portguard.Set(m, why, ":7000") // frps 默认 bindPort：frpc 连它做注册，不该被穿透
	return m
}

// frpDir 某实例的 frpc 工作目录。
//
// **不是实例目录**：frpc.toml 决定"把哪些本地端口挂到哪个 frps 上"，而 frpc 以
// root 运行 —— 让实例用户能改写这份配置，等于允许他把节点上任意本地端口
// （22/SSH、9091/Daemon gRPC）挂到自己的 frps 上对外暴露。
// 因此它属于平台状态，放在 root 0700 的 frp_state_dir 下。
func (s *Server) frpDir(instanceID string) string {
	if s.cfg != nil && s.cfg.FrpStateDir != "" {
		return filepath.Join(s.cfg.FrpStateDir, instanceID)
	}
	if s.cfg != nil {
		return filepath.Join(filepath.Dir(filepath.Clean(s.cfg.InstanceDir)), "frp", instanceID)
	}
	return ""
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
