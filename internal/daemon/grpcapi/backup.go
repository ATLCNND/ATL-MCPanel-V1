package grpcapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/mcprocess"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

const backupsDirName = "backups"

// skipDirs 备份时始终排除的顶层目录（可重新生成 / 体积大 / 内部数据）。
var skipDirs = map[string]bool{
	"logs":       true,
	"backups":    true,
	"cache":      true,
	"libraries":  true,
	"versions":   true,
	"crash-reports": true,
}

// configEntries include_config=false 时排除的配置项（保留纯世界数据）。
var configEntries = map[string]bool{
	"server.properties":     true,
	"bukkit.yml":            true,
	"spigot.yml":            true,
	"paper.yml":             true,
	"paper-global.yml":      true,
	"paper-world-defaults.yml": true,
	"commands.yml":          true,
	"permissions.yml":       true,
	"help.yml":              true,
	"ops.json":              true,
	"whitelist.json":        true,
	"banned-players.json":   true,
	"banned-ips.json":       true,
	"usercache.json":        true,
	"version_history.json":  true,
	"eula.txt":              true,
	"config":                true,
	"plugins":               true,
	"mods":                  true,
	"mods-disabled":         true,
}

var unsafeNameRe = regexp.MustCompile(`[^A-Za-z0-9._\-\x{4e00}-\x{9fa5}]+`)

// backupDir 返回实例的备份目录（并确保存在）。
//
// 解析顺序（前者优先）：
//  1. 实例自身的 backup_dir —— 允许单个实例单独指定存放位置
//  2. 节点配置的 backup_root —— 把所有备份集中到另一块盘（如机械盘冷存储）
//  3. 实例目录下的 backups/ —— 默认行为（与之前版本兼容）
//
// 把备份放到独立磁盘的实际意义：备份是顺序读写、对随机 IO 无要求，
// 用大容量机械盘承载可以显著降低每 GB 成本，同时避免备份挤占
// 实例所在 SSD 的空间。
func (s *Server) backupDir(inst *mcprocess.Instance) (string, error) {
	var d string
	switch {
	case inst.BackupDir != "":
		d = inst.BackupDir
	case s.cfg.BackupRoot != "":
		// 按实例分目录，避免不同实例的备份混在一起
		d = filepath.Join(s.cfg.BackupRoot, sanitizeDirName(inst.ID))
	default:
		d = filepath.Join(inst.Dir, backupsDirName)
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// sanitizeDirName 保证实例 ID 可安全用作目录名（防止路径穿越）。
func sanitizeDirName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "instance"
	}
	return b.String()
}

// Backup 创建实例备份（tar.gz）。
func (s *Server) Backup(ctx context.Context, req *pb.BackupRequest) (*pb.BackupResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.BackupResponse{Success: false, Message: "实例不存在"}, nil
	}
	dir := inst.Dir

	// 运行中：先触发存档落盘，尽量保证一致性
	if inst.Status() == "running" {
		s.flushWorld(inst)
	}

	bdir, err := s.backupDir(inst)
	if err != nil {
		return &pb.BackupResponse{Success: false, Message: err.Error()}, nil
	}

	name := sanitizeName(req.Name)
	now := time.Now()
	if name == "" {
		name = "auto"
	}
	fileName := fmt.Sprintf("bk_%d_%s.tar.gz", now.Unix(), name)
	target := filepath.Join(bdir, fileName)

	f, err := os.Create(target)
	if err != nil {
		return &pb.BackupResponse{Success: false, Message: "创建备份文件失败: " + err.Error()}, nil
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := writeInstanceTar(tw, dir, req.IncludeConfig); err != nil {
		tw.Close()
		gz.Close()
		_ = os.Remove(target)
		return &pb.BackupResponse{Success: false, Message: "打包失败: " + err.Error()}, nil
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		return &pb.BackupResponse{Success: false, Message: "打包收尾失败: " + err.Error()}, nil
	}
	if err := gz.Close(); err != nil {
		return &pb.BackupResponse{Success: false, Message: "压缩收尾失败: " + err.Error()}, nil
	}

	fi, _ := os.Stat(target)
	s.log.Info("备份完成", "instance", req.InstanceId, "file", fileName, "size", fi.Size())
	return &pb.BackupResponse{
		Success:  true,
		BackupId: fileName,
		Message:  fmt.Sprintf("备份完成（%.1f MB）", float64(fi.Size())/1024/1024),
	}, nil
}

// flushWorld 通过控制台触发存档落盘，尽量保证备份一致性。
// 接管状态的实例没有 stdin，无法发送命令，仅能直接打包（可能不是最新存档）。
func (s *Server) flushWorld(inst *mcprocess.Instance) {
	if inst.Adopted() {
		s.log.Warn("实例处于接管状态，无法触发存档落盘，备份可能不含最新存档")
		return
	}
	if err := inst.SendCommand("save-all flush"); err != nil {
		s.log.Warn("save-all flush 失败", "error", err)
		return
	}
	time.Sleep(1200 * time.Millisecond) // 给存档落盘留出时间
}

// writeInstanceTar 将实例目录写入 tar（按规则过滤）。
func writeInstanceTar(tw *tar.Writer, root string, includeConfig bool) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		top := strings.SplitN(rel, "/", 2)[0]

		// 排除内部目录
		if skipDirs[top] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// 纯世界备份：排除配置项
		if !includeConfig {
			if configEntries[top] {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// 会话锁等运行时文件跳过
			if info.Name() == "session.lock" {
				return nil
			}
		}

		// 符号链接等非常规文件跳过
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
}

// ListBackups 列出备份。
func (s *Server) ListBackups(ctx context.Context, req *pb.ListBackupsRequest) (*pb.ListBackupsResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.ListBackupsResponse{Success: false, Error: "实例不存在"}, nil
	}
	bdir, err := s.backupDir(inst)
	if err != nil {
		return &pb.ListBackupsResponse{Success: true, Backups: []*pb.BackupInfo{}}, nil
	}
	entries, err := os.ReadDir(bdir)
	if err != nil {
		return &pb.ListBackupsResponse{Success: true, Backups: []*pb.BackupInfo{}}, nil
	}

	list := make([]*pb.BackupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name, created := parseBackupName(e.Name(), info.ModTime())
		list = append(list, &pb.BackupInfo{
			BackupId:      e.Name(),
			Name:          name,
			Size:          info.Size(),
			CreatedAt:     created,
			IncludeConfig: true,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt > list[j].CreatedAt })
	return &pb.ListBackupsResponse{Success: true, Backups: list}, nil
}

// DeleteBackup 删除备份。
func (s *Server) DeleteBackup(ctx context.Context, req *pb.DeleteBackupRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	bdir, err := s.backupDir(inst)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	target, err := safeBackupPath(bdir, req.BackupId)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	if err := os.Remove(target); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.OperationResponse{Success: true, Message: "备份已删除"}, nil
}

// Restore 从备份恢复实例（需先停止；恢复后需手动启动）。
func (s *Server) Restore(ctx context.Context, req *pb.RestoreRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	if inst.Status() == "running" {
		return &pb.OperationResponse{Success: false, Error: "实例运行中，请先停止再回滚"}, nil
	}

	bdir, err := s.backupDir(inst)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	src, err := safeBackupPath(bdir, req.BackupId)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	f, err := os.Open(src)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: "打开备份失败: " + err.Error()}, nil
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: "备份文件损坏: " + err.Error()}, nil
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	root := inst.Dir
	base, _ := filepath.Abs(root)
	count := 0

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &pb.OperationResponse{Success: false, Error: "读取备份失败: " + err.Error()}, nil
		}

		// 防目录穿越
		clean := filepath.Clean("/" + hdr.Name)
		target := filepath.Join(root, clean)
		abs, _ := filepath.Abs(target)
		if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
			return &pb.OperationResponse{Success: false, Error: "备份包含非法路径: " + hdr.Name}, nil
		}
		if strings.HasPrefix(filepath.ToSlash(clean), "/"+backupsDirName+"/") {
			continue // 不恢复备份目录自身
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o644)
			if err != nil {
				return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return &pb.OperationResponse{Success: false, Error: "写入文件失败: " + err.Error()}, nil
			}
			out.Close()
			count++
		}
	}

	s.log.Info("回滚完成", "instance", req.InstanceId, "backup", req.BackupId, "files", count)
	return &pb.OperationResponse{Success: true, Message: fmt.Sprintf("回滚完成，已恢复 %d 个文件（请启动实例）", count)}, nil
}

// ---- 辅助 ----

// safeBackupPath 校验 backup_id 并返回安全路径。
// safeBackupPath 把 backup_id 安全地拼到备份目录下。
// baseDir 必须是已经解析好的备份目录（可能位于独立磁盘上）。
func safeBackupPath(baseDir, backupID string) (string, error) {
	if backupID == "" {
		return "", fmt.Errorf("backup_id 不能为空")
	}
	if strings.ContainsAny(backupID, `/\`) || strings.Contains(backupID, "..") {
		return "", fmt.Errorf("非法的 backup_id")
	}
	if !strings.HasSuffix(backupID, ".tar.gz") {
		return "", fmt.Errorf("非法的备份文件")
	}
	// baseDir 是已解析好的备份目录（可能位于独立磁盘上）
	return filepath.Join(baseDir, filepath.Base(backupID)), nil
}

// sanitizeName 清理备份名称中的不安全字符。
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = unsafeNameRe.ReplaceAllString(s, "_")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// parseBackupName 从文件名解析名称与创建时间。
func parseBackupName(fileName string, fallback time.Time) (string, int64) {
	base := strings.TrimSuffix(fileName, ".tar.gz")
	parts := strings.SplitN(base, "_", 3)
	if len(parts) >= 3 && parts[0] == "bk" {
		if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			return parts[2], ts
		}
	}
	return base, fallback.Unix()
}
