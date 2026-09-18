package grpcapi

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/runas"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// protectedFiles 实例目录中不通过文件接口暴露的内部文件。
//
// protectedFiles 受保护文件。
//
// 这些文件由面板/穿透引擎自动生成并维护，属于**平台级内部状态**：
//   - frpc.toml 含 frps 地址与 auth token（共享密钥，泄露即可在 frps 上自建隧道）
//   - tunnels.json 同样含 frps 信息与隧道定义
//   - frpc.pid 为进程管理文件，改动会破坏隧道状态跟踪
//   - instance.json 为实例元数据（核心类型、jar 路径、启停统计、配额）。
//     其中 name 字段属于**面板侧**的显示名：改名的权威数据在面板数据库里，
//     Daemon 这份是建实例时写下的快照、之后不会再更新。留在文件管理里只会
//     造成"两个名字对不上"的困惑，因此一并归入内部状态。
//
// 实例协作者（collab，只读即可）理应看不到它们：穿透由管理员在
// 「穿透管理」中统一分配，无需也不应让租户接触 frp 凭据。
var protectedFiles = map[string]bool{
	"frpc.toml":     true,
	"tunnels.json":  true,
	"frpc.pid":      true,
	"instance.json": true,
}

// protectedLogFiles 受保护目录下的敏感日志（相对实例目录，使用 / 分隔）。
var protectedLogFiles = map[string]bool{
	"logs/frpc.log": true,
}

// errProtected 受保护文件的统一拒绝信息。
// 不透露文件是否存在及其内容，仅说明由面板统一管理。
const errProtected = "该文件由面板统一管理（穿透配置），不支持通过文件管理访问"

// metaFileName 实例元数据文件名（物理位置在平台状态目录，见 config.StateDir）。
const metaFileName = "instance.json"

// isInstanceMetaPath 判断请求路径是否指向实例元数据文件。
//
// 允许带前导 "/" 或 "./"：面板不同代码路径里写法不完全一致，
// 而这里只需要认出一个固定文件名，没必要让调用方先规范化。
func isInstanceMetaPath(p string) bool {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+strings.TrimPrefix(p, "/"))), "/")
	return clean == metaFileName
}

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
		// instance.json 例外：它**读**得到、改不了。
		//
		// 原因：它的物理位置已经搬到平台状态目录（root 0700），但面板的
		// 「启动脚本」等功能仍按"实例内路径"读它（拿 jar 路径与内存参数）。
		// 与其让面板换一套数据来源（那样共享 jar 目录的场景会退化），
		// 不如在这里做一个**只读虚拟映射**：读走状态目录里的那份真文件，
		// 而写/删/复制仍然被 isProtectedPath 挡住 —— 那份文件决定资源限制
		// 与接管行为，不该由文件管理面篡改。
		if isInstanceMetaPath(req.Path) {
			metaPath := filepath.Join(s.reg.StateDir(req.InstanceId), metaFileName)
			data, err := os.ReadFile(metaPath)
			if err != nil {
				return &pb.ReadFileResponse{Success: false, Error: "实例元数据不存在"}, nil
			}
			return &pb.ReadFileResponse{Success: true, Content: string(data), Size: int64(len(data))}, nil
		}
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
	// 交给实例的运行用户：root 写出来的文件属主是 root，而真正要读写它的
	// 是实例进程（另一个 uid）—— 不改属主，服务端下次改写这个文件就会
	// permission denied（面板里改完配置、服务器却说没权限）。
	s.handOver(req.InstanceId, target)
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
	// 目录也要交出去：实例用户要往里面写（插件建 data 目录、服务端建 world_nether）
	s.handOver(req.InstanceId, target)
	return &pb.OperationResponse{Success: true, Message: "创建成功"}, nil
}

// handOver 把刚写入的路径交给实例的运行用户（属主改成它）。
//
// 三条不变量，缺一条都会出问题：
//
//  1. **只处理实例目录内的路径**。handOver 的语义是"把租户自己的文件交给租户"，
//     一旦落到实例目录之外（例如配了 backup_root 的冷存储备份），
//     改属主就等于把平台的备份交到租户手里 —— 那种备份"租户删不掉"才是它的意义。
//     所以越界的路径直接返回，而不是"尽力而为"。
//
//  2. **目标本身 + 新创建出来的上级目录都要交**。只 chown 目标是不够的：
//     写 plugins/Foo/config.yml 时，plugins/ 与 plugins/Foo/ 都是 Daemon 刚用
//     MkdirAll 建出来的（属主 root），插件随后要在 Foo/ 里建 data/ 就会被拒。
//     因此从目标父目录一路上溯到实例目录，逐级改属主。
//
//  3. **失败只记日志、不改变调用方的成功语义**。改属主失败最坏是"实例之后写不了
//     这个文件"，而写入本身已经成功了；把一个已经落盘的写入报成失败，
//     只会让用户重试出一堆重复文件。真正的问题在日志里，且下次启动实例时
//     整棵 chown 会把它纠正过来。
//
// 静默通过的情形：实例不托管、非 root 且拿不到身份（那种情况下进程与 Daemon
// 同 uid，属主本来就不是问题）、路径不存在（删除类操作之后调用是正常的）。
func (s *Server) handOver(instanceID, path string) {
	if path == "" {
		return
	}
	inst, ok := s.reg.Get(instanceID)
	if !ok {
		return
	}
	root := filepath.Clean(inst.Dir)
	target := filepath.Clean(path)
	if !isSubPath(root, target) {
		return // 不变量 1
	}
	id, err := s.reg.Identity(instanceID)
	if err != nil || id == nil {
		return
	}
	if runas.ChownTree(target, id) != nil {
		s.log.Warn("把文件交给实例运行用户失败，实例可能无法写入它",
			"instance", instanceID, "path", target, "user", id.Username)
	}

	for dir := filepath.Dir(target); dir != root && isSubPath(root, dir); dir = filepath.Dir(dir) {
		runas.Chown(dir, id)
	}
}
