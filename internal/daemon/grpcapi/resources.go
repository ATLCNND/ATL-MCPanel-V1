package grpcapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/javaruntime"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/resources"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ListResources 列出节点上的共享资源，并标注每个资源被哪些实例引用。
//
// 之所以要带引用信息：删除一个仍被实例引用的 jar 会让那个实例下次启动
// 直接失败（"jar 不存在"），而失败要等到有人手动重启才暴露。
// 提前把引用列出来，管理员才能判断"这个能不能删"。
func (s *Server) ListResources(ctx context.Context, req *pb.EmptyRequest) (*pb.ListResourcesResponse, error) {
	if s.res == nil {
		return &pb.ListResourcesResponse{Success: false, Error: "未配置节点资源目录（daemon.resource_dir）"}, nil
	}
	files, err := s.res.List()
	if err != nil {
		return &pb.ListResourcesResponse{Success: false, Error: err.Error()}, nil
	}

	byPath := s.jarReferences()

	out := make([]*pb.ResourceInfo, 0, len(files))
	for _, f := range files {
		out = append(out, &pb.ResourceInfo{
			Name:    f.Name,
			Path:    f.Path,
			Size:    f.Size,
			ModTime: f.ModTime,
			Refs:    byPath[normalizePath(f.Path)],
		})
	}
	return &pb.ListResourcesResponse{Success: true, Dir: s.res.Dir(), Files: out}, nil
}

// UploadResource 上传一个共享资源（管理员操作，权限由面板侧把关）。
func (s *Server) UploadResource(ctx context.Context, req *pb.UploadResourceRequest) (*pb.OperationResponse, error) {
	if s.res == nil {
		return &pb.OperationResponse{Success: false, Error: "未配置节点资源目录（daemon.resource_dir）"}, nil
	}
	name := strings.TrimSpace(req.Filename)
	if name == "" {
		return &pb.OperationResponse{Success: false, Error: "文件名不能为空"}, nil
	}

	// 同名且未允许覆盖时直接拒绝：悄悄覆盖会让正在引用该资源的实例
	// 在下次重启时行为突变（换了核心却不自知）
	if !req.Overwrite {
		if _, err := os.Stat(filepath.Join(s.res.Dir(), name)); err == nil {
			return &pb.OperationResponse{
				Success: false,
				Code:    "name_conflict",
				Error:   "同名资源已存在：" + name + "（如需替换请勾选「覆盖同名资源」）",
			}, nil
		}
	}

	f, err := s.res.Save(name, req.Content)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	s.log.Info("共享资源已上传", "name", f.Name, "size", resources.HumanSize(f.Size), "dir", s.res.Dir())
	return &pb.OperationResponse{
		Success: true,
		Message: "已上传 " + f.Name + "（" + resources.HumanSize(f.Size) + "）",
	}, nil
}

// DeleteResource 删除共享资源。
//
// 默认拦下仍被实例引用的资源：那是"删完之后某天某个实例起不来"的典型来源。
// force 为真时放行，但调用方（面板）必须先让管理员明确确认。
func (s *Server) DeleteResource(ctx context.Context, req *pb.DeleteResourceRequest) (*pb.OperationResponse, error) {
	if s.res == nil {
		return &pb.OperationResponse{Success: false, Error: "未配置节点资源目录（daemon.resource_dir）"}, nil
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return &pb.OperationResponse{Success: false, Error: "文件名不能为空"}, nil
	}

	users := s.jarReferences()[normalizePath(filepath.Join(s.res.Dir(), name))]
	if len(users) > 0 && !req.Force {
		return &pb.OperationResponse{
			Success: false,
			Error:   "该资源正被实例引用：" + strings.Join(users, "、") + "（删除后这些实例将无法启动）",
		}, nil
	}

	if err := s.res.Remove(name); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	s.log.Warn("共享资源已删除", "name", name, "referenced_by", strings.Join(users, ","))
	return &pb.OperationResponse{Success: true, Message: "已删除 " + name}, nil
}

// jarReferences 建立 "jar 绝对路径 -> 引用它的实例 ID 列表" 的反查表。
// 一次遍历所有实例，避免每个资源都扫一遍。
func (s *Server) jarReferences() map[string][]string {
	out := map[string][]string{}
	for _, id := range s.reg.List() {
		inst, ok := s.reg.Get(id)
		if !ok || inst.JarPath == "" {
			continue
		}
		key := normalizePath(inst.JarPath)
		out[key] = append(out[key], id)
	}
	return out
}

// ListJavaRuntimes 列出节点上实际安装的 JDK。
//
// 这条 RPC 存在的意义：让"Java 版本"从一个纯记录字段变成真正生效的选择 ——
// 面板只列出节点上真实可用、可执行的 java，选中哪个启动就用哪个。
func (s *Server) ListJavaRuntimes(ctx context.Context, req *pb.EmptyRequest) (*pb.ListJavaRuntimesResponse, error) {
	rts := javaruntime.Discover()
	out := make([]*pb.JavaRuntimeInfo, 0, len(rts))
	for _, r := range rts {
		out = append(out, &pb.JavaRuntimeInfo{
			Label:   r.Label,
			Path:    r.Path,
			Home:    r.Home,
			Version: r.Version,
		})
	}
	return &pb.ListJavaRuntimesResponse{Success: true, Runtimes: out, Fallback: javaruntime.Fallback()}, nil
}

// normalizePath 统一路径形式，用于比较"实例的 jar 路径"与"资源路径"。
// 实例里的 jar 路径可能是相对的（旧数据），而资源路径一定是绝对的。
func normalizePath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}
