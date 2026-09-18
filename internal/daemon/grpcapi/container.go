package grpcapi

import (
	"context"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/container"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 容器化隔离（可选）：节点能力查询 + 按实例开关。
//
// 这两个接口存在的意义是让面板能把"能不能开容器化"这件事**如实**显示给管理员：
// 节点没装 docker、镜像没导入、实例正在运行，都是明说的拒绝理由，
// 而不是让开关看起来可点、点了却没生效。

// GetContainerCapability 查询本节点的容器化能力。
func (s *Server) GetContainerCapability(ctx context.Context, _ *pb.EmptyRequest) (*pb.ContainerCapability, error) {
	rt := s.reg.ContainerRuntime()
	if rt == nil {
		return &pb.ContainerCapability{
			Available:     false,
			DockerPresent: false,
			Reason:        "该节点未安装 docker（或已在配置里关闭容器化），无法使用容器化隔离",
		}, nil
	}

	ver := rt.Version(ctx)
	res := &pb.ContainerCapability{
		DockerPresent: ver != "",
		DockerVersion: ver,
		Image:         rt.Image(),
	}
	if !res.DockerPresent {
		res.Reason = "docker 已安装但服务端无应答（docker 没起来？），无法使用容器化隔离"
		return res, nil
	}
	res.ImagePresent = rt.ImageLoaded(ctx)
	res.Available = res.ImagePresent
	if !res.ImagePresent {
		res.Reason = "基础镜像 " + rt.Image() + " 未导入：" +
			"请把部署包里的 atl-mcpanel-runtime-*.tar.gz 用 `docker load -i` 导入（install.sh 会自动做）"
	}
	return res, nil
}

// SetInstanceContainer 开关某实例的容器化隔离（下次启动生效）。
func (s *Server) SetInstanceContainer(ctx context.Context, req *pb.SetInstanceContainerRequest) (*pb.OperationResponse, error) {
	if req.InstanceId == "" {
		return &pb.OperationResponse{Success: false, Error: "缺少实例 ID"}, nil
	}
	if err := s.reg.SetContainerMode(req.InstanceId, req.Enabled); err != nil {
		s.log.Warn("修改容器化设置失败", "instance", req.InstanceId, "enabled", req.Enabled, "error", err)
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	state := "已关闭容器化（实例将直接运行在节点上）"
	if req.Enabled {
		state = "已开启容器化：实例将运行在容器 " + container.NameOf(req.InstanceId) + " 里（下次启动生效）"
	}
	s.log.Info("实例容器化设置已更新", "instance", req.InstanceId, "enabled", req.Enabled)
	return &pb.OperationResponse{Success: true, Message: state}, nil
}
