package grpcapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// maxJarSize 单个 jar 的大小上限。
//
// 与共享资源、gRPC 消息上限共用同一个常量：这三者原本各写各的
// （256MB / 512MB / gRPC 默认 4MB），结果就是传一个 41MB 的 jar
// 会撞上传输层的 4MB 限制，报出一句与真实原因无关的 ResourceExhausted。
const maxJarSize = grpclimits.MaxUploadBytes

// ListJars 列出实例目录中的核心 jar，并标注当前使用的那个。
func (s *Server) ListJars(ctx context.Context, req *pb.ListJarsRequest) (*pb.ListJarsResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.ListJarsResponse{Success: false, Error: "实例不存在"}, nil
	}

	active := inst.JarPath
	out := []*pb.JarInfo{}

	entries, err := os.ReadDir(inst.Dir)
	if err != nil {
		return &pb.ListJarsResponse{Success: false, Error: err.Error()}, nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".jar") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, &pb.JarInfo{
			Filename: e.Name(),
			Path:     e.Name(),
			Size:     info.Size(),
			Active:   samePath(active, filepath.Join(inst.Dir, e.Name())),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })

	return &pb.ListJarsResponse{Success: true, Jars: out, ActiveJar: active}, nil
}

// UploadJar 上传核心 jar 到实例目录。
func (s *Server) UploadJar(ctx context.Context, req *pb.UploadJarRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	if len(req.Content) == 0 {
		return &pb.OperationResponse{Success: false, Error: "jar 内容为空"}, nil
	}
	if len(req.Content) > maxJarSize {
		return &pb.OperationResponse{Success: false,
			Error: fmt.Sprintf("jar 过大（%d 字节），上限 %d 字节", len(req.Content), maxJarSize)}, nil
	}

	name := filepath.Base(strings.TrimSpace(req.Filename))
	if name == "" || name == "." || name == ".." {
		return &pb.OperationResponse{Success: false, Error: "文件名无效"}, nil
	}
	if !strings.HasSuffix(strings.ToLower(name), ".jar") {
		return &pb.OperationResponse{Success: false, Error: "仅支持 .jar 文件"}, nil
	}

	dst := filepath.Join(inst.Dir, name)
	// 防止通过文件名穿越目录
	if !strings.HasPrefix(filepath.Clean(dst), filepath.Clean(inst.Dir)+string(os.PathSeparator)) {
		return &pb.OperationResponse{Success: false, Error: "目标路径非法"}, nil
	}

	if err := os.WriteFile(dst, req.Content, 0o644); err != nil {
		return &pb.OperationResponse{Success: false, Error: "写入 jar 失败: " + err.Error()}, nil
	}
	// jar 是**实例进程要读**的文件（java -jar）：root 写出来的 jar 若属主是 root、
	// 模式又是 0644，实例用户仍能读；但用户之后用「核心管理」重新上传/替换时会
	// 撞上"目录属主不是自己"的删除权限问题 —— 一并交出去，边界只有一处。
	s.handOver(req.InstanceId, dst)

	s.log.Info("核心 jar 已上传", "instance", req.InstanceId, "file", name, "size", len(req.Content))
	return &pb.OperationResponse{Success: true,
		Message: fmt.Sprintf("已上传 %s（%.1f MB）", name, float64(len(req.Content))/1024/1024)}, nil
}

// SetInstanceJar 切换实例使用的核心 jar（重启后生效）。
//
// 支持两种写法：
//   - 相对路径/文件名：在实例目录内查找（如 folia.jar）
//   - 绝对路径：允许指向实例目录之外的共享 jar 目录
//     （实例创建时使用的 jar 常位于共享目录，必须能切回）
func (s *Server) SetInstanceJar(ctx context.Context, req *pb.SetInstanceJarRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}

	raw := strings.TrimSpace(req.JarPath)
	if raw == "" {
		return &pb.OperationResponse{Success: false, Error: "jar 路径不能为空"}, nil
	}
	if !strings.HasSuffix(strings.ToLower(raw), ".jar") {
		return &pb.OperationResponse{Success: false, Error: "仅支持 .jar 文件"}, nil
	}

	var full string
	if filepath.IsAbs(raw) {
		full = filepath.Clean(raw)
	} else {
		// 仅允许实例目录内的相对路径，避免 ../ 穿越
		name := filepath.Base(raw)
		if name == "." || name == ".." {
			return &pb.OperationResponse{Success: false, Error: "jar 路径无效"}, nil
		}
		full = filepath.Join(inst.Dir, name)
	}

	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		return &pb.OperationResponse{Success: false, Error: "jar 文件不存在: " + full}, nil
	}

	if err := s.reg.SetJarPath(req.InstanceId, full); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	inst.SetJarPath(full)

	s.log.Info("核心 jar 已切换", "instance", req.InstanceId, "jar", full)
	return &pb.OperationResponse{Success: true,
		Message: "核心已切换为 " + filepath.Base(full) + "，重启实例后生效"}, nil
}

// samePath 比较两个路径是否指向同一文件（忽略绝对/相对差异）。
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return aa == bb
}
