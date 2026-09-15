package grpcapi

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// protectedFiles 实例目录中不通过文件接口暴露的内部文件。
//
// 这些文件由面板/穿透引擎自动生成并维护，属于**平台级内部状态**：
//   - frpc.toml 含 frps 地址与 auth token（共享密钥，泄露即可在 frps 上自建隧道）
//   - tunnels.json 同样含 frps 信息与隧道定义
//   - frpc.pid 为进程管理文件，改动会破坏隧道状态跟踪
//
// 实例协作者（collab，只读即可）理应看不到它们：穿透由管理员在
// 「穿透管理」中统一分配，无需也不应让租户接触 frp 凭据。
var protectedFiles = map[string]bool{
	"frpc.toml":    true,
	"tunnels.json": true,
	"frpc.pid":     true,
}

// protectedLogFiles 受保护目录下的敏感日志（相对实例目录，使用 / 分隔）。
var protectedLogFiles = map[string]bool{
	"logs/frpc.log": true,
}

// errProtected 受保护文件的统一拒绝信息。
// 不透露文件是否存在及其内容，仅说明由面板统一管理。
const errProtected = "该文件由面板统一管理（穿透配置），不支持通过文件管理访问"

// isProtectedPath 判断实例内相对路径是否为受保护的内部文件。
func isProtectedPath(rel string) bool {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+strings.TrimPrefix(rel, "/"))), "/")
	if protectedFiles[clean] {
		return true
	}
	if protectedLogFiles[clean] {
		return true
	}
	return false
}

// isProtectedName 判断某个目录项是否为受保护文件（用于列表过滤）。
// dirRel 为该项所在目录的实例相对路径。
func isProtectedName(dirRel, name string) bool {
	rel := strings.TrimPrefix(filepath.ToSlash(filepath.Join(dirRel, name)), "/")
	return isProtectedPath(rel)
}

// resolvePath 解析实例相对路径为绝对路径，并防止目录穿越。
func resolvePath(instanceDir, rel string) (string, error) {
	rel = filepath.Clean("/" + strings.TrimPrefix(rel, "/"))
	abs := filepath.Join(instanceDir, rel)
	// 确保在实例目录内
	base, _ := filepath.Abs(instanceDir)
	target, _ := filepath.Abs(abs)
	if !strings.HasPrefix(target, base+string(filepath.Separator)) && target != base {
		return "", os.ErrPermission
	}
	return target, nil
}

func (s *Server) instanceDir(instanceID string) (string, error) {
	inst, ok := s.reg.Get(instanceID)
	if !ok {
		return "", &notFoundError{id: instanceID}
	}
	return inst.Dir, nil
}

type notFoundError struct{ id string }

func (e *notFoundError) Error() string { return "实例 " + e.id + " 不存在" }

// ListFiles 列出目录。
func (s *Server) ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.ListFilesResponse{Success: false, Error: err.Error()}, nil
	}
	target, err := resolvePath(dir, req.Path)
	if err != nil {
		return &pb.ListFilesResponse{Success: false, Error: err.Error()}, nil
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return &pb.ListFilesResponse{Success: false, Error: err.Error()}, nil
	}

	files := make([]*pb.FileInfo, 0, len(entries))
	for _, e := range entries {
		// 隐藏面板内部文件（frp 凭据等），避免通过文件管理泄露
		if isProtectedName(req.Path, e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, &pb.FileInfo{
			Name:    e.Name(),
			Path:    filepath.ToSlash(strings.TrimPrefix(filepath.Join(req.Path, e.Name()), "/")),
			IsDir:   e.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
		})
	}
	// 目录在前，按名称排序
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return files[i].Name < files[j].Name
	})
	return &pb.ListFilesResponse{Success: true, Files: files}, nil
}

// ReadFile 读取文件内容（限文本文件，最大 1MB）。
func (s *Server) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.ReadFileResponse{Success: false, Error: err.Error()}, nil
	}
	if isProtectedPath(req.Path) {
		return &pb.ReadFileResponse{Success: false, Error: errProtected}, nil
	}
	target, err := resolvePath(dir, req.Path)
	if err != nil {
		return &pb.ReadFileResponse{Success: false, Error: err.Error()}, nil
	}
	info, err := os.Stat(target)
	if err != nil {
		return &pb.ReadFileResponse{Success: false, Error: err.Error()}, nil
	}
	if info.IsDir() {
		return &pb.ReadFileResponse{Success: false, Error: "是目录，无法读取"}, nil
	}
	if info.Size() > 1<<20 {
		return &pb.ReadFileResponse{Success: false, Error: "文件过大（>1MB）"}, nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return &pb.ReadFileResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.ReadFileResponse{Success: true, Content: string(data), Size: info.Size()}, nil
}

// WriteFile 写入文件。
func (s *Server) WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*pb.OperationResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	if isProtectedPath(req.Path) {
		return &pb.OperationResponse{Success: false, Error: errProtected}, nil
	}
	target, err := resolvePath(dir, req.Path)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	if err := os.WriteFile(target, []byte(req.Content), 0o644); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.OperationResponse{Success: true, Message: "写入成功"}, nil
}

// DeleteFile 删除文件/目录。
func (s *Server) DeleteFile(ctx context.Context, req *pb.FileRequest) (*pb.OperationResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	// 禁止删除实例根目录本身
	if req.Path == "" || req.Path == "/" || req.Path == "." {
		return &pb.OperationResponse{Success: false, Error: "不能删除实例根目录"}, nil
	}
	if isProtectedPath(req.Path) {
		return &pb.OperationResponse{Success: false, Error: errProtected}, nil
	}
	target, err := resolvePath(dir, req.Path)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}

	// 禁止删除实例当前使用的核心 jar：删除后实例将无法再次启动，
	// 且问题要等到下次重启才暴露，属于难以排查的隐患。
	if inst, ok := s.reg.Get(req.InstanceId); ok && samePath(inst.JarPath, target) {
		return &pb.OperationResponse{Success: false,
			Error: "该文件是实例当前使用的核心 jar，请先切换到其它核心后再删除"}, nil
	}

	if err := os.RemoveAll(target); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.OperationResponse{Success: true, Message: "删除成功"}, nil
}

// Mkdir 创建目录。
func (s *Server) Mkdir(ctx context.Context, req *pb.MkdirRequest) (*pb.OperationResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	target, err := resolvePath(dir, req.Path)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.OperationResponse{Success: true, Message: "创建成功"}, nil
}