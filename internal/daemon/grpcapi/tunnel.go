package grpcapi

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/portguard"
	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ApplyTunnel 应用（新增/更新）一条穿透隧道：生成 frpc 配置并拉起 frpc。
func (s *Server) ApplyTunnel(ctx context.Context, req *pb.TunnelRequest) (*pb.OperationResponse, error) {
	if req.InstanceId == "" || req.TunnelId == "" {
		return &pb.OperationResponse{Success: false, Error: "instance_id 与 tunnel_id 必填"}, nil
	}
	// 公网直出（底账 F16）：这条隧道**不走 frpc**，端口由实例自己对宿主机发布。
	// 所以它不需要 frps 信息，也不生成 frpc 配置；要做的只有两件事：
	//   ① 记下"该实例要对外发布端口"，让容器在下次启动时发到 0.0.0.0；
	//   ② 如实回执 —— 没在跑的实例要告诉管理员"重启后才生效"。
	if req.PublishMode == "direct" {
		return s.applyDirectTunnel(req)
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
		// 第三方线路的登录方式与租户标识（底账 F16）
		AuthMode: req.FrpsAuthMode,
		User:     req.FrpsUser,
	}, frp.Tunnel{
		TunnelID:      req.TunnelId,
		Name:          name,
		Protocol:      req.Protocol,
		LocalPort:     int(req.LocalPort),
		RemotePort:    int(req.RemotePort),
		Subdomain:     req.Subdomain,
		CustomDomains: splitDomains(req.CustomDomain),
		UseTLS:        req.UseTls,
		// 服务商分配的代理名与传输参数，必须原样使用（改了/缺了都注册不上）
		ProxyName:          req.ProxyName,
		BandwidthLimit:     req.BandwidthLimit,
		BandwidthLimitMode: req.BandwidthLimitMode,
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

// applyDirectTunnel 处理公网直出隧道：不生成 frpc 配置，只把"该实例要对外发布端口"记下来。
//
// 为什么要记在实例上而不是隧道里：端口发布方式是在 **docker run 那一刻**定死的
//（容器的端口映射不能事后改），所以必须在实例**启动前**就知道；
// 而隧道是独立的对象，实例启动时不会去挨个查隧道。
func (s *Server) applyDirectTunnel(req *pb.TunnelRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	// 只标记"对外发布"，**不动**容器化开关：直连与容器化是可以并存的
	//（容器化 + 端口对外发布），把它关掉反而是错的。
	inst.SetPublishPublic(true)
	// 同时登记进 frp manager 的持久化状态（tunnels.json）——
	// 这是**跨 daemon 重启**唯一能活下来的地方；只放内存的话，
	// 升级 daemon 之后实例再启动就会退回"只发布到回环"，直连静默失效。
	if err := s.frp.ApplyDirect(req.InstanceId, s.frpDir(req.InstanceId), req.TunnelId, int(req.LocalPort)); err != nil {
		s.log.Warn("登记直连隧道失败", "instance", req.InstanceId, "tunnel", req.TunnelId, "error", err)
	}

	running := inst.Status() == "running"
	msg := "公网直出已登记：玩家直接连接节点公网 IP:" + itoa32(req.LocalPort)
	if inst.Containerized() {
		if running {
			msg += "（容器端口需要重启实例后才会对外发布）"
		} else {
			msg += "（该实例是容器化的，已在下次启动时按对外发布处理）"
		}
	} else if !running {
		msg += "（实例未运行，启动后端口即可访问）"
	}
	s.log.Info("公网直出隧道已登记", "instance", req.InstanceId, "tunnel", req.TunnelId,
		"local_port", req.LocalPort, "containerized", inst.Containerized(), "running", running)
	return &pb.OperationResponse{Success: true, Message: msg}, nil
}

// splitDomains 把面板传来的域名串（可能是逗号分隔的多个）拆成切片。
//
// 面板里的 custom_domain 是一列字符串，而 frpc 要的是数组；用逗号在线上传最简单，
// 免得为一个字段再改一次 proto。空串与空白项直接丢掉。
func splitDomains(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func itoa32(n int32) string { return strconv.Itoa(int(n)) }
