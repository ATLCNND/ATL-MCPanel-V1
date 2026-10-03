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

	"github.com/ATLCNND/ATL-MCPanel/internal/common/safepath"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/mcprocess"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

const backupsDirName = "backups"

// skipDirs 备份时始终排除的顶层目录（可重新生成 / 体积大 / 内部数据）。
var skipDirs = map[string]bool{
	"logs":          true,
	"backups":       true,
	"cache":         true,
	"libraries":     true,
	"versions":      true,
	"crash-reports": true,
}

// configEntries include_config=false 时排除的配置项（保留纯世界数据）。
var configEntries = map[string]bool{
	"server.properties":        true,
	"bukkit.yml":               true,
	"spigot.yml":               true,
	"paper.yml":                true,
	"paper-global.yml":         true,
	"paper-world-defaults.yml": true,
	"commands.yml":             true,
	"permissions.yml":          true,
	"help.yml":                 true,
	"ops.json":                 true,
	"whitelist.json":           true,
	"banned-players.json":      true,
	"banned-ips.json":          true,
	"usercache.json":           true,
	"version_history.json":     true,
	"eula.txt":                 true,
	"config":                   true,
	"plugins":                  true,
	"mods":                     true,
	"mods-disabled":            true,
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
//
// ⚠️ inst.BackupDir **来自建实例请求里的 backup_dir 字段（客户端可控）**，
// 因此不能无条件采信（见 instanceBackupDirAllowed）。
func (s *Server) backupDir(inst *mcprocess.Instance) (string, error) {
	var d string
	switch {
	case inst.BackupDir != "":
		allowed, err := s.instanceBackupDirAllowed(inst)
		if err != nil {
			return "", err
		}
		d = allowed
	case s.cfg.BackupRoot != "":
		// 按实例分目录，避免不同实例的备份混在一起
		d = filepath.Join(s.cfg.BackupRoot, sanitizeDirName(inst.ID))
	default:
		d = filepath.Join(inst.Dir, backupsDirName)
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	// 只有**实例目录内**的备份目录才交给实例用户（那是租户自己的文件，
	// 他要能下载、重命名、删除）。配了 backup_root 放到独立磁盘时不交 ——
	// 那份属于平台冷存储，"租户改不了"才是它的意义（否则备份就成了
	// 可以被一并删掉的东西）。Daemon 是 root，两种情况都照样能读写；
	// handOver 自己会拒绝越界路径，这里不必再判一次。
	//
	// 顺带记一笔"为什么越界的 backup_dir 以前没人发现"：正是因为它**静默**
	// 拒绝越界路径（file.go 的不变量 1：不属于实例目录就直接返回，连日志都不记），
	// 所以租户把备份指向别的实例时既不报错、也没人看见 —— 一切看起来都正常。
	s.handOver(inst.ID, d)
	return d, nil
}

// instanceBackupDirAllowed 校验实例自带的 backup_dir 并返回它。
//
// inst.BackupDir 直接来自面板建实例请求里的 `backup_dir` 字段（**客户端可控**），
// 而它随后会被当作 Backup（以 root 建文件）、ListBackups、DeleteBackup
//（os.Remove 以 root）、Restore（读文件并解到实例目录）的根目录。
// 于是一个普通租户只要建实例时填 "/opt/atl-node/instances/<别人的实例>/backups"：
// 列出、下载、删除别人的备份，再恢复到自己的实例里 —— 别人的 world、ops.json、
// server.properties 全成了可读的；顺带还拿到"以 root 在任意目录建/删 *.tar.gz"
// 的能力。
//
// 所以只认两种位置：① 实例自己的目录之内；② 节点配置的 backup_root 之下
//（后者是管理员在节点配置文件里写的、可信的集中存储）。
//
// 两道检查都要做：先词法边界（Within），再真实路径边界（ResolveWithin）——
// 软链接同样能把备份目录指到实例之外（`backups -> /etc`）。
//
// 不满足时的处理**选的是"报错"而不是"悄悄回退到默认位置"**：
// 回退会让备份落到面板以为之外的地方（旧的备份文件看着"消失了"，
// 保留策略也会算错），排查成本比一条明确的错误高得多；而这里其它失败
//（MkdirAll 失败等）本来也是返回错误的，报错与文件内既有风格一致。
//
// TODO(面板侧)：面板不应再接受实例级 backup_dir 这个字段 —— 它由普通租户填写，
// 却决定 root 读写哪些文件。本函数只是 Daemon 侧的兜底，不改变面板的接口。
func (s *Server) instanceBackupDirAllowed(inst *mcprocess.Instance) (string, error) {
	raw := inst.BackupDir
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("实例配置的备份目录 %s 不是绝对路径，已拒绝使用", raw)
	}
	// ① 实例目录内（词法 + 真实路径两道）
	if safepath.Within(inst.Dir, raw) {
		if _, err := safepath.ResolveWithin(inst.Dir, raw); err == nil {
			return raw, nil
		}
	}
	// ② 节点配置的 backup_root 之下（同样两道）
	if s.cfg != nil && s.cfg.BackupRoot != "" && safepath.Within(s.cfg.BackupRoot, raw) {
		if _, err := safepath.ResolveWithin(s.cfg.BackupRoot, raw); err == nil {
			return raw, nil
		}
	}
	return "", fmt.Errorf(
		"实例配置的备份目录 %s 不在允许范围内（只允许实例目录内或节点 backup_root 之下），已拒绝使用",
		raw)
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
	// ⚠️ 空名 = 自动备份。这条约定是**隐式契约**：面板的保留策略按"名字是不是 auto"
	// 区分手动/自动，从而把手动备份排除在梯度淘汰之外。
	// 面板的定时计划现在**显式**传 "auto"；手动路径（用户点「立即备份」）在面板侧
	// 生成 "手动-<时间>"，并拒绝把名字填成 auto。这里保留默认值只为兼容老调用方。
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
	// 备份文件本身也交出去（同样只在实例目录内时才会生效，见 handOver 的不变量）
	s.handOver(req.InstanceId, target)

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

	root := inst.Dir
	base, _ := filepath.Abs(root)

	gz, err := gzip.NewReader(f)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: "备份文件损坏: " + err.Error()}, nil
	}
	defer gz.Close()

	// 空间预检：解压**后**的体积无法从 gzip 头部可靠得知（ISIZE 只有 4 字节、
	// 还是 mod 2^32 的），所以这里只做一个下限检查 —— 至少要能放下备份文件本身
	// 再加上 uploadFreeMargin 的余量，避免"磁盘已经快满了还往里灌"。
	// 真正的上限靠下面边写边累计的字节上限兜住。
	// 读不到容量（Statfs 失败）就不拦：不能因为一次探测失败把回滚整体挡住。
	if fi, serr := f.Stat(); serr == nil {
		if free, ferr := freeBytes(root); ferr == nil && !enoughSpace(free, fi.Size()) {
			return &pb.OperationResponse{Success: false, Error: fmt.Sprintf(
				"节点磁盘剩余空间不足：备份 %s，当前可用 %s（已预留 %s 余量）",
				humanSize(fi.Size()), humanSize(free), humanSize(uploadFreeMargin))}, nil
		}
	}

	tr := tar.NewReader(gz)
	count := 0
	// 与解压（fileops.Extract）同一套资源上限：回滚以前一条都没有，
	// 于是一个 1MB 的"全零 gzip"能解出几百 GB、把节点磁盘写满，
	// 同节点**其它租户**的实例跟着一起存不了档。
	quota := restoreQuota{maxBytes: maxRestoreBytes, maxFiles: maxRestoreFiles, maxDepth: maxRestoreDepth}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &pb.OperationResponse{Success: false, Error: "读取备份失败: " + err.Error()}, nil
		}
		if err := quota.entry(hdr.Name); err != nil {
			return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
		}

		// 防目录穿越（词法）
		clean := filepath.Clean("/" + hdr.Name)
		target := filepath.Join(root, clean)
		abs, _ := filepath.Abs(target)
		if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
			return &pb.OperationResponse{Success: false, Error: "备份包含非法路径: " + hdr.Name}, nil
		}
		// 再加一道**真实路径**（解析软链接）检查：`backups/` 不是受保护路径，
		// 租户可以在自己的实例里放 `d/evil -> /etc/cron.d`，再让备份文件里带
		// 条目 `d/evil/rce` —— 词法路径看着在实例目录内，实际却写到实例之外
		//（Daemon 是 root）。解压那条路（fileops.safeJoin）与这里是同一个口子。
		// 解析结果只用于判成败，target 仍用词法拼出来的那个，行为与修复前一致。
		if _, err := safepath.ResolveWithin(base, abs); err != nil {
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
			// O_NOFOLLOW：拒绝把软链接当目标写（TOCTOU 窗口也一并关掉）
			out, err := safepath.OpenNoFollow(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o644)
			if err != nil {
				if safepath.IsSymlinkRefusal(err) {
					return &pb.OperationResponse{Success: false,
						Error: "备份条目目标是软链接，已拒绝写入: " + hdr.Name}, nil
				}
				return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
			}
			// 边写边累计：多读 1 字节是为了区分"正好等于上限"与"超限"
			n, cerr := io.Copy(out, io.LimitReader(tr, quota.limit()))
			out.Close()
			if err := quota.add(n); err != nil {
				return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
			}
			if cerr != nil {
				return &pb.OperationResponse{Success: false, Error: "写入文件失败: " + cerr.Error()}, nil
			}
			count++
		}
	}

	s.log.Info("回滚完成", "instance", req.InstanceId, "backup", req.BackupId, "files", count)
	// 回滚出来的整棵目录树都要交给实例用户：这是最危险的一处遗漏 ——
	// 恢复出来的 world/ 属主是 root 的话，服务端下次保存世界就会
	// permission denied，而用户看到的是"回滚成功了，但服务器起不来/存不了档"。
	s.handOver(req.InstanceId, root)
	return &pb.OperationResponse{Success: true, Message: fmt.Sprintf("回滚完成，已恢复 %d 个文件（请启动实例）", count)}, nil
}

// ---- 辅助 ----

// 回滚（Restore）的资源上限。
//
// 与 fileops/archive.go 里的 maxExtractBytes / maxExtractFiles / maxExtractDepth
// **取同一组值**：那三个常量在 fileops 包里未导出、这里拿不到，只能照抄一份 ——
// fileops/archive.go 是这组数字的唯一权威来源，改动那边时要一起改。
//
// 为什么必须有：解压（Extract）早就有这三道上限，而**回滚一条都没有**，
// 于是同一类输入换条路就能绕过 —— 一个约 1MB 的"全零 gzip"（tar 里放若干条
// 声明尺寸巨大的条目）能解出几百 GB，把节点磁盘写满，同节点上**其它租户**的
// 实例跟着一起存不了档、写不了日志。
const (
	maxRestoreBytes = int64(200) << 30 // 200 GB
	maxRestoreFiles = 500000           // 条目数上限
	maxRestoreDepth = 64               // 目录层级上限
)

// restoreQuota 回滚过程中的资源计数（条目数与累计写入字节）。
//
// 单独做成一个小类型而不是在 Restore 里散着几个变量，是为了**可测**：
// 200 GB 的上限没法在测试里真的写出来，但把上限换成小值就能验证
// "超限即拒绝"这条路径确实走到了（见 backup_security_test.go）。
type restoreQuota struct {
	maxBytes int64
	maxFiles int
	maxDepth int

	entries int
	bytes   int64
}

// entry 记一个归档条目，并检查条目数与目录层级上限。
func (q *restoreQuota) entry(name string) error {
	if q.entries+1 > q.maxFiles {
		return fmt.Errorf("备份条目过多（> %d）", q.maxFiles)
	}
	if d := archiveDepth(name); d > q.maxDepth {
		return fmt.Errorf("备份目录层级过深（%s）", name)
	}
	q.entries++
	return nil
}

// add 累计本次写入的字节数，并检查总量上限。
//
// 先判后加（被拒绝时不改计数）：否则"多读 1 字节"的那一次超限会把计数推过上限，
// limit() 随即算成 0 —— 读侧从此一个字节都读不到，而且失败的操作**改变了状态**
// 本身就是这类计数器最容易出错的写法（TestRestoreQuotaCaps 锁住了这条）。
func (q *restoreQuota) add(n int64) error {
	if q.bytes+n > q.maxBytes {
		return fmt.Errorf("回滚体积超过上限（%d GB）", q.maxBytes>>30)
	}
	q.bytes += n
	return nil
}

// limit 返回本次还允许读取的字节数（多读 1 字节用于判断"是否正好超限"）。
func (q *restoreQuota) limit() int64 { return q.maxBytes - q.bytes + 1 }

// archiveDepth 归档条目的目录层级，与 fileops.safeJoin 的 maxExtractDepth 同口径
//（它数的是路径段数，所以这里把条目名按 "/" 的个数来数，含最前面那个）。
func archiveDepth(name string) int {
	return strings.Count(filepath.ToSlash(filepath.Clean("/"+name)), "/")
}

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
	// 按**字符**截断而不是按字节：中文一个字 3 字节，`s[:40]` 会把第 14 个字
	// 切成半个 UTF-8 序列 —— 文件名里留下非法字节，界面与 JSON 里显示成乱码。
	// 保留期长的备份（比如"开荒前-正式服-第一周目"这类长中文名）很容易撞到 40。
	if r := []rune(s); len(r) > maxBackupNameRunes {
		s = string(r[:maxBackupNameRunes])
	}
	return s
}

// maxBackupNameRunes 备份名的长度上限（按字符计）。
// 40 是历史值（当时按字节算），改成按字符后含义不变、但对中文名更宽松。
const maxBackupNameRunes = 40

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
