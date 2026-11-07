package grpcapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// 增量链的基线策略（docs/DESIGN-INCREMENTAL-BACKUP.md §3.4）。
const (
	// maxChainDepth 一条链最多允许的环数（含基线）：到 6 环就写新基线。
	// 链越长恢复越慢，而链上**任一环**坏了整条链就废 —— 环数上限是控制这个风险的手段。
	maxChainDepth = 6
	// maxChainAge 基线最多复用 7 天，超了就重建。
	// 与环数上限互补：一天只备份一次时环数永远到不了 6，靠这一条兜住。
	maxChainAge = 7 * 24 * time.Hour
	// maxChainWalk 往回走链时的硬上限。清单是磁盘上的普通文件（可能被手工改过），
	// prev_id 形成环时必须能停下来，否则备份会挂在这里无穷循环。
	maxChainWalk = 64
)

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
	return s.backupDirWith(inst, "")
}

// backupDirWith 同 backupDir，但允许指定**本次**的备份根目录（面板下发的节点级覆盖值）。
//
// 覆盖值不享受任何特权：必须是绝对路径 —— 相对路径的含义取决于当前工作目录，
// 而备份落错地方的后果是「以为有备份、其实没有」。仍按实例分目录，避免混在一起。
func (s *Server) backupDirWith(inst *mcprocess.Instance, override string) (string, error) {
	var d string
	switch {
	case override != "":
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("备份根目录必须是绝对路径，收到 %q", override)
		}
		d = filepath.Join(override, sanitizeDirName(inst.ID))
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
// （os.Remove 以 root）、Restore（读文件并解到实例目录）的根目录。
// 于是一个普通租户只要建实例时填 "/opt/atl-node/instances/<别人的实例>/backups"：
// 列出、下载、删除别人的备份，再恢复到自己的实例里 —— 别人的 world、ops.json、
// server.properties 全成了可读的；顺带还拿到"以 root 在任意目录建/删 *.tar.gz"
// 的能力。
//
// 所以只认两种位置：① 实例自己的目录之内；② 节点配置的 backup_root 之下
// （后者是管理员在节点配置文件里写的、可信的集中存储）。
//
// 两道检查都要做：先词法边界（Within），再真实路径边界（ResolveWithin）——
// 软链接同样能把备份目录指到实例之外（`backups -> /etc`）。
//
// 不满足时的处理**选的是"报错"而不是"悄悄回退到默认位置"**：
// 回退会让备份落到面板以为之外的地方（旧的备份文件看着"消失了"，
// 保留策略也会算错），排查成本比一条明确的错误高得多；而这里其它失败
// （MkdirAll 失败等）本来也是返回错误的，报错与文件内既有风格一致。
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

	// 面板可下发本次的备份根目录（管理员改备份位置后立即生效，不必重启 daemon）
	bdir, err := s.backupDirWith(inst, req.BackupRoot)
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
	// 这个名字现在有两重作用：既决定会不会被保留策略淘汰，也决定它进不进增量链。
	// 所以判断必须是**精确匹配**（理由见 isAutoBackupName）。
	isAuto := isAutoBackupName(name)

	// 文件名里的 unix 秒是**链的排序依据**（latestChainManifest 按它找链尾）与
	// 上一环的定位依据，同一秒内的第二份备份必须往后挪，不能覆盖（见 uniqueBackupFileName）。
	fileName, err := uniqueBackupFileName(bdir, now.Unix(), name)
	if err != nil {
		return &pb.BackupResponse{Success: false, Message: err.Error()}, nil
	}
	target := filepath.Join(bdir, fileName)

	// ---- 增量决策（第 2d 步：scanEntries + latestChainManifest + changedSince）----
	//
	// 三条不变式（docs/DESIGN-INCREMENTAL-BACKUP.md §3、交接文档 §5.2）：
	//  1. 清单记录**本轮完整状态**，不是"这次打包了什么" —— 否则下一轮会把没打包的
	//     文件误判为已删除，恢复时真的删掉。
	//  2. 链只认 `_auto`：手动备份随时可能被用户删，不能当基准。
	//  3. 新基线条件：环数 ≥ 6 或距上次基线 > 7 天（另加"链不完整"）。
	current, scanErr := scanEntries(dir, req.IncludeConfig)
	if scanErr != nil {
		// 扫不出完整状态就**不能**做增量：拿一份不完整的清单当基准，等于亲手埋下
		// "下一轮把没扫到的文件判成已删除"的雷。
		s.log.Warn("扫描实例目录失败，本次退回完整基线", "instance", req.InstanceId, "error", scanErr)
	}
	dec := s.decideChain(bdir, fileName, isAuto, scanErr == nil, current, req.IncludeConfig, now)
	if isAuto && dec.Kind == "base" {
		s.log.Info("本次写新基线", "instance", req.InstanceId, "why", dec.Why)
	}

	f, err := os.Create(target)
	if err != nil {
		return &pb.BackupResponse{Success: false, Message: "创建备份文件失败: " + err.Error()}, nil
	}
	defer f.Close()
	// 备份文件本身也交出去（同样只在实例目录内时才会生效，见 handOver 的不变量）
	s.handOver(req.InstanceId, target)

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	entries, terr := writeInstanceTar(tw, dir, req.IncludeConfig, dec.Include)
	if err := terr; err != nil {
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

	// 写清单（第 1 步就有的地基；2d 起区分 base / inc 并补 prev/base/墓碑）。
	// 清单失败不影响备份可用 —— 它只影响「这条备份能否作为增量链的基准」。
	archiveSum, sumErr := fileSHA256(target)
	if sumErr != nil {
		s.log.Warn("计算备份包哈希失败", "instance", req.InstanceId, "error", sumErr)
	}
	// 清单的 files 用**扫描结果**，而不是打包时看到的文件列表：
	// 若某个文件恰好在"扫描之后、打包之前"被改，打包看到的已是新签名 ——
	// 把它记进清单，下一轮就会认为"和上一轮一样"而永远不再打包它（静默丢文件）。
	// 记着扫描时的旧签名则相反：下一轮判定"变了"，再打一次包。宁可多打一次，不能漏。
	// 扫描失败时只能退回打包时看到的状态（那一轮本来就是完整基线）。
	files := current
	if scanErr != nil {
		files = entries
	}
	if files == nil {
		files = []manifestFile{}
	}
	deleted := dec.Deleted
	if deleted == nil {
		deleted = []string{}
	}
	includeCfg := req.IncludeConfig
	manifest := backupManifest{
		Schema:        1,
		Kind:          dec.Kind,
		InstanceID:    req.InstanceId,
		CreatedAt:     now,
		BackupRoot:    bdir,
		BaseID:        dec.BaseID,
		PrevID:        dec.PrevID,
		IncludeConfig: &includeCfg,
		Files:         files,
		Deleted:       deleted,
		ArchiveSHA256: archiveSum,
	}
	if err := writeManifest(target+".json", manifest); err != nil {
		s.log.Warn("写备份清单失败（备份本身可用）", "instance", req.InstanceId, "error", err)
	}

	kindText := "完整"
	if dec.Kind == "inc" {
		kindText = "增量"
	}
	s.log.Info("备份完成", "instance", req.InstanceId, "file", fileName, "kind", dec.Kind,
		"size", fi.Size(), "files", len(files), "changed", dec.Packed, "deleted", len(deleted))
	return &pb.BackupResponse{
		Success:  true,
		BackupId: fileName,
		Message:  fmt.Sprintf("备份完成（%s，%.1f MB）", kindText, float64(fi.Size())/1024/1024),
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
// include 决定某个文件是否**写进包**（nil = 全写）。
//
// 注意它只影响「打包」，不影响「记录」：清单里的 files 必须是**本轮完整状态**，
// 否则下一轮拿它做对比时会把没写进包的文件当成「已删除」。
func writeInstanceTar(tw *tar.Writer, root string, includeConfig bool, include func(rel string) bool) ([]manifestFile, error) {
	var entries []manifestFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
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

		// 排除规则与 scanEntries 共用一份（见 excludedTopLevel 的说明）
		if excludedTopLevel(rel, info, includeConfig) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// 目录一律写头部（空目录无害）；文件按 include 过滤
		if !info.IsDir() && include != nil && !include(rel) {
			// 不进包，但仍要记进清单（见上面的说明）
			if info.Mode().IsRegular() {
				entries = append(entries, manifestFile{Path: rel, Size: info.Size(), Mtime: info.ModTime().Unix()})
			}
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
		if _, err = io.Copy(tw, src); err != nil {
			return err
		}
		// 记进清单：只记 path/size/mtime（第 1 步不算每文件哈希，理由见清单结构注释）
		entries = append(entries, manifestFile{
			Path:  rel,
			Size:  info.Size(),
			Mtime: info.ModTime().Unix(),
		})
		return nil
	})
	return entries, err
}

// ListBackups 列出备份。
//
// 目录解析必须与 Backup 用**同一个** backupDirWith：备份写到哪，就要从哪读回来。
// （2026-11-07 的 bug：这里只走 backupDir，于是节点配了冷存储之后，
// 面板的备份列表里看不见任何一份实际备份 —— 列表还停在实例目录下那堆老的。）
func (s *Server) ListBackups(ctx context.Context, req *pb.ListBackupsRequest) (*pb.ListBackupsResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.ListBackupsResponse{Success: false, Error: "实例不存在"}, nil
	}
	bdir, err := s.backupDirWith(inst, req.BackupRoot)
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
		bi := &pb.BackupInfo{
			BackupId:      e.Name(),
			Name:          name,
			Size:          info.Size(),
			CreatedAt:     created,
			IncludeConfig: true,
		}
		// 链信息供面板做**整链淘汰**：老备份没有清单 → 两个字段留空，视同独立的一份。
		if m, ok := readManifestFile(filepath.Join(bdir, e.Name())); ok {
			bi.Kind = m.Kind
			bi.BaseId = m.BaseID
		}
		list = append(list, bi)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt > list[j].CreatedAt })
	return &pb.ListBackupsResponse{Success: true, Backups: list}, nil
}

// readManifestFile 读某个包旁边的清单（读不到/不合法就当没有）。
func readManifestFile(tarPath string) (*backupManifest, bool) {
	raw, err := os.ReadFile(tarPath + ".json")
	if err != nil {
		return nil, false
	}
	var m backupManifest
	if json.Unmarshal(raw, &m) != nil || m.Schema != 1 {
		return nil, false
	}
	return &m, true
}

// DeleteBackup 删除备份。
//
// 包和它的清单一起删：只删 .tar.gz 会留下一堆孤儿 .json，
// 而「清单在、包没了」恰好是 daemon 判定"链坏了"的形态 —— 除了噪声没有好处。
func (s *Server) DeleteBackup(ctx context.Context, req *pb.DeleteBackupRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	bdir, err := s.backupDirWith(inst, req.BackupRoot)
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
	_ = os.Remove(target + ".json")
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

	bdir, err := s.backupDirWith(inst, req.BackupRoot)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	src, err := safeBackupPath(bdir, req.BackupId)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}

	// 增量第 3 步：恢复的输入从「一个包」变成「一条链」（base → inc* → 墓碑）。
	// 老备份/手动备份没有链（清单缺 prev_id 或干脆没清单）→ refs 只有它自己，
	// 走下面的单包路径，行为与以前完全一致。
	refs, cerr := s.restoreChainOf(bdir, src)
	if cerr != nil {
		return &pb.OperationResponse{Success: false, Error: cerr.Error()}, nil
	}
	if len(refs) > 1 {
		res, rerr := s.restoreChain(inst, refs)
		if rerr != nil {
			return &pb.OperationResponse{Success: false, Error: rerr.Error()}, nil
		}
		s.log.Info("链式回滚完成", "instance", req.InstanceId, "backup", req.BackupId,
			"packages", res.Packages, "files", res.Files, "deleted", res.Tombs)
		// 与单包回滚同一个收尾：整棵树交给实例用户，否则服务端下次存档就 permission denied
		s.handOver(req.InstanceId, inst.Dir)
		return &pb.OperationResponse{Success: true, Message: fmt.Sprintf(
			"回滚完成：链 %d 环，恢复 %d 个文件，删除 %d 个（请启动实例）",
			res.Packages, res.Files, res.Tombs)}, nil
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
// （它数的是路径段数，所以这里把条目名按 "/" 的个数来数，含最前面那个）。
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

// ---- 链式恢复（增量第 3 步，见 DESIGN-INCREMENTAL-BACKUP.md §3.3）----
//
// 恢复的输入从「一个包 → 一份目录」变成「一条链 → 一份目录」：
//
//	校验每个包的 archive_sha256 → 全部解到暂存目录 → 覆盖到实例目录 → 应用墓碑
//
// 为什么要先解到暂存目录：链上任何一环坏了，**一步都不能动实例目录**。
// 边解边覆盖的写法下，若第三个包损坏，实例里已经是前两个包的混合状态 ——
// 用户这时候手里的「回滚」其实把事情变得更糟，而且没有任何提示。

// chainRef 链上的一环：包名 + 它的清单（老备份没有清单时为 nil）。
type chainRef struct {
	Name     string // 包文件名（同时是清单名去掉 .json）
	Path     string // 绝对路径
	Manifest *backupManifest
}

// restoreChainOf 从 target 往回走，返回 base → target 顺序的链。
//
// 缺环直接报错，不做「能恢复多少算多少」：残缺的链恢复出来的实例看着正常、
// 实际少了东西，用户会在很久以后才发现 —— 那比恢复失败糟得多。
func (s *Server) restoreChainOf(bdir, target string) ([]chainRef, error) {
	name := filepath.Base(target)
	seen := make(map[string]bool, 8)
	var refs []chainRef
	for len(refs) <= maxChainWalk {
		if seen[name] {
			return nil, fmt.Errorf("备份链存在环（%s 重复出现），已拒绝恢复", name)
		}
		seen[name] = true
		path := filepath.Join(bdir, name)
		if _, err := os.Stat(path); err != nil {
			if len(refs) == 0 {
				return nil, fmt.Errorf("备份不存在: %s", name)
			}
			return nil, fmt.Errorf("备份链不完整：缺少 %s，无法回滚到这一环", name)
		}
		var m *backupManifest
		if raw, rerr := os.ReadFile(path + ".json"); rerr == nil {
			var mm backupManifest
			if json.Unmarshal(raw, &mm) == nil && mm.Schema == 1 {
				m = &mm
			}
		}
		refs = append(refs, chainRef{Name: name, Path: path, Manifest: m})
		if m == nil || m.PrevID == "" {
			break // 到底了：base，或者没有清单的老备份/手动备份 = 单包
		}
		name = m.PrevID
	}
	if len(refs) > maxChainWalk {
		return nil, fmt.Errorf("备份链超过 %d 环，已拒绝恢复（清单可能被改过）", maxChainWalk)
	}
	// 逆序成 base → target
	for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
		refs[i], refs[j] = refs[j], refs[i]
	}
	return refs, nil
}

// chainNeedBytes 估算按链还原需要多少空间：取**最后那一环清单的完整状态**总量
// （清单记的是整轮完整状态，所以它就是一个完整实例的大小），没有清单时退回压缩包大小之和。
func chainNeedBytes(refs []chainRef) int64 {
	for i := len(refs) - 1; i >= 0; i-- {
		m := refs[i].Manifest
		if m == nil || len(m.Files) == 0 {
			continue
		}
		var sum int64
		for _, f := range m.Files {
			sum += f.Size
		}
		return sum
	}
	var sum int64
	for _, ref := range refs {
		if fi, err := os.Stat(ref.Path); err == nil {
			sum += fi.Size()
		}
	}
	return sum
}

// extractPackage 把一个包解到暂存目录，返回它实际包含的普通文件（相对路径）。
//
// 与就地恢复的区别只在**落点**：这里写的是我们刚建、root 0700、绝不含软链接的暂存目录，
// 所以不需要 O_NOFOLLOW；真正的「写进租户目录」那一步在 applyStaging 里，那里仍走
// safepath.OpenNoFollow + 两道路径边界。quota 由调用方共享，跨包累计。
func extractPackage(tarPath, dest string, quota *restoreQuota) ([]string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("打开备份失败: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("备份文件损坏: %w", err)
	}
	defer gz.Close()
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var files []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取备份失败: %w", err)
		}
		if err := quota.entry(hdr.Name); err != nil {
			return nil, err
		}
		clean := filepath.ToSlash(filepath.Clean("/" + hdr.Name))
		rel := strings.TrimPrefix(clean, "/")
		if rel == "" || rel == backupsDirName || strings.HasPrefix(rel, backupsDirName+"/") {
			continue // 不恢复备份目录自身
		}
		target := filepath.Join(absDest, filepath.FromSlash(rel))
		// 词法边界：tar 条目可以带 ../（暂存目录是新的，没有软链接问题，但越界仍要挡）
		if target != absDest && !strings.HasPrefix(target, absDest+string(filepath.Separator)) {
			return nil, fmt.Errorf("备份包含非法路径: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o644)
			if err != nil {
				return nil, err
			}
			n, cerr := io.Copy(out, io.LimitReader(tr, quota.limit()))
			out.Close()
			if err := quota.add(n); err != nil {
				return nil, err
			}
			if cerr != nil {
				return nil, fmt.Errorf("写入文件失败: %w", cerr)
			}
			files = append(files, rel)
		}
	}
	return files, nil
}

// safeInstanceTarget 把清单/归档里的相对路径解析成实例目录内的绝对路径（两道边界）。
//
// 两道都要做，理由与单包恢复相同：`backups/` 不是受保护路径，租户可以在自己的实例里
// 放 `d/evil -> /etc/cron.d`，词法路径看着在实例目录内，实际却写到实例之外（Daemon 是 root）。
func safeInstanceTarget(base, root, rel string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean("/" + rel))
	rel = strings.TrimPrefix(clean, "/")
	if rel == "" {
		return "", fmt.Errorf("非法空路径")
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	abs, _ := filepath.Abs(target)
	if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return "", fmt.Errorf("备份包含非法路径: %s", rel)
	}
	if _, err := safepath.ResolveWithin(base, abs); err != nil {
		return "", fmt.Errorf("备份包含非法路径: %s", rel)
	}
	return target, nil
}

// chainRestoreResult 一次链式恢复的结果（日志与回应文案用）。
type chainRestoreResult struct {
	Packages int
	Files    int
	Tombs    int
}

// restoreChain 按链还原实例目录：校验 → 解到暂存 → 覆盖 → 应用墓碑。
func (s *Server) restoreChain(inst *mcprocess.Instance, refs []chainRef) (chainRestoreResult, error) {
	res := chainRestoreResult{Packages: len(refs)}
	root := inst.Dir
	base, _ := filepath.Abs(root)

	// ① 完整性校验：任何一个包对不上，就一步都不动实例目录。
	for _, ref := range refs {
		if ref.Manifest == nil || ref.Manifest.ArchiveSHA256 == "" {
			continue // 老备份没记整包哈希：没有可比基准，不等于"通过"
		}
		sum, err := fileSHA256(ref.Path)
		if err != nil {
			return res, fmt.Errorf("校验备份 %s 失败: %w", ref.Name, err)
		}
		if !strings.EqualFold(sum, ref.Manifest.ArchiveSHA256) {
			return res, fmt.Errorf("备份 %s 校验失败（sha256 与清单不一致），已拒绝恢复", ref.Name)
		}
	}

	// ② 空间预检：暂存的是一份完整实例，覆盖时实例目录里还会有一份，按两份算。
	need := chainNeedBytes(refs)
	if free, err := freeBytes(root); err == nil && need > 0 && !enoughSpace(free, need*2) {
		return res, fmt.Errorf("节点磁盘剩余空间不足：按链还原约需 %s，当前可用 %s（已预留 %s 余量）",
			humanSize(need*2), humanSize(free), humanSize(uploadFreeMargin))
	}

	// ③ 暂存目录放在实例目录**旁边**（同一文件系统、实例之外）：
	//    不在实例里，就不会被扫描/打包看到，租户也够不着。
	staging := filepath.Join(filepath.Dir(root), fmt.Sprintf(".restore-%s-%d",
		sanitizeDirName(inst.ID), time.Now().UnixNano()))
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return res, fmt.Errorf("创建暂存目录失败: %w", err)
	}
	defer os.RemoveAll(staging)

	// ④ 逐包解到暂存目录；顺便算出**净最终状态**（后一环覆盖前一环，墓碑记成"不存在"）。
	quota := restoreQuota{maxBytes: maxRestoreBytes, maxFiles: maxRestoreFiles, maxDepth: maxRestoreDepth}
	final := make(map[string]bool, 1024)
	for _, ref := range refs {
		files, err := extractPackage(ref.Path, staging, &quota)
		if err != nil {
			return res, fmt.Errorf("解包 %s 失败: %w", ref.Name, err)
		}
		for _, p := range files {
			final[p] = true
		}
		if ref.Manifest != nil {
			for _, p := range ref.Manifest.Deleted {
				if p == "" {
					continue
				}
				clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+p)), "/")
				final[clean] = false
			}
		}
	}

	// ⑤ 覆盖：只写"最终应该存在"的文件，逐文件 O_NOFOLLOW 写进实例目录。
	err := filepath.WalkDir(staging, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(staging, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(rel, backupsDirName+"/") {
			return nil
		}
		if d.IsDir() {
			return nil // 目录由下面写文件时按需创建
		}
		if keep, ok := final[rel]; ok && !keep {
			return nil // 墓碑：这一轮不写，第 ⑥ 步删
		}
		target, terr := safeInstanceTarget(base, root, rel)
		if terr != nil {
			return terr
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, serr := os.Open(path)
		if serr != nil {
			return serr
		}
		defer src.Close()
		mode := os.FileMode(0o644)
		if fi, serr := src.Stat(); serr == nil {
			mode = fi.Mode() & 0o644
		}
		out, oerr := safepath.OpenNoFollow(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if oerr != nil {
			if safepath.IsSymlinkRefusal(oerr) {
				return fmt.Errorf("备份条目目标是软链接，已拒绝写入: %s", rel)
			}
			return oerr
		}
		_, cerr := io.Copy(out, src)
		out.Close()
		if cerr != nil {
			return fmt.Errorf("写入文件失败: %s: %w", rel, cerr)
		}
		res.Files++
		return nil
	})
	if err != nil {
		return res, err
	}

	// ⑥ 应用墓碑：链上明确"删过且之后没再加回来"的路径，从实例目录删掉。
	//    墓碑是**唯一**允许删除用户数据的入口，所以只认清单里写明的路径，不做目录扫描比对。
	for rel, keep := range final {
		if keep || rel == "" || strings.HasPrefix(rel, backupsDirName+"/") {
			continue
		}
		target, terr := safeInstanceTarget(base, root, rel)
		if terr != nil {
			return res, terr
		}
		if rmErr := os.Remove(target); rmErr != nil && !os.IsNotExist(rmErr) {
			return res, fmt.Errorf("删除墓碑 %s 失败: %w", rel, rmErr)
		}
		res.Tombs++
	}
	return res, nil
}

// ---- 备份清单（增量备份的地基，见 docs/DESIGN-INCREMENTAL-BACKUP.md）----
//
// 为什么到第 2d 步仍只记 path/size/mtime、不记每文件哈希：
// 判据（changedSince）用的是 (path,size,mtime)，而逐文件算 sha256 等于每次备份多读
// 一遍盘 —— 备份最大的成本就是这份 IO。每文件哈希要等**链式恢复**（第 3 步）落地时
// 一起接上，那时它才有用处（恢复前逐文件校验），也才值得这份代价。
// 整包哈希现在就算 —— 它只读一遍刚写好的包，代价小，且能立刻用于完整性校验。

type backupManifest struct {
	Schema        int            `json:"schema"`
	Kind          string         `json:"kind"` // base（完整快照）| inc（只含自上一环以来变化的文件）
	InstanceID    string         `json:"instance_id"`
	CreatedAt     time.Time      `json:"created_at"`
	BackupRoot    string         `json:"backup_root,omitempty"`
	BaseID        string         `json:"base_id,omitempty"`        // inc：链上的基线包；base：它自己
	PrevID        string         `json:"prev_id,omitempty"`        // inc：上一环包；base：空（链到此为止）
	IncludeConfig *bool          `json:"include_config,omitempty"` // 备份范围；与上一环不同就不能接链
	Files         []manifestFile `json:"files"`                    // **本轮完整状态**，不是"这次打包了什么"
	Deleted       []string       `json:"deleted"`                  // 墓碑：上一环有、本轮没有
	ArchiveSHA256 string         `json:"archive_sha256"`
}

type manifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"`
	SHA256 string `json:"sha256,omitempty"` // 第 2 步起对变化文件填充
}

// writeManifest 在备份包旁边写一份清单（同名 .json）。
//
// 清单写失败**不影响备份本身**：包已经落盘、可用；清单缺失只意味着「这条备份
// 暂时不能作为增量链的基准」，不是因为一个 json 就让用户丢一次备份。
func writeManifest(path string, m backupManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// fileSHA256 计算文件哈希（第 2 步起用于「变化判据」，这里先备好）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- 增量判据与链（第 2d 步起已接入 Backup 主流程）----
//
// 为什么先把判据单独实现并测透：判据错了的后果是**静默丢文件** —— 备份看起来成功、
// 恢复后才发现少了东西。这种错误不能靠「跑一次看看」发现，必须用用例钉住。

// autoBackupName 自动备份的固定名字（面板与节点之间的约定，见 panel/httpapi/backups.go）。
const autoBackupName = "auto"

// isAutoBackupName 这个名字是不是"自动备份"。
//
// 必须**精确**匹配（忽略大小写与首尾空白），不能写成 strings.HasPrefix(name, "auto")：
// 名字是区分手动/自动的**唯一**手段，而面板允许用户把手动备份起名成 "autosave"、
// "自动化前测试"。前缀匹配会把这类手动备份当成自动备份，于是
//  1. 它们会被写成增量而不是完整快照 —— 违反"手动备份永远完整"（DESIGN §5）；
//  2. 用户删掉其中一份，整条增量链就断了。
//
// 面板侧用的是同一条规则（internal/panel/httpapi/backups.go 的 isAutoBackupName），
// 两边必须一致，否则"界面上算手动、节点上算自动"。
func isAutoBackupName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), autoBackupName)
}

// uniqueBackupFileName 给本次备份取一个**不与已有文件重名**的名字。
//
// 名字里的 unix 秒不只是给人看的：latestChainManifest 按它找链尾，chainState 顺着
// 文件名定位上一环。同一秒内的第二份备份若沿用同一个名字，os.Create 会**静默截断**
// 上一份 —— 链上的一环变成了它自己，直到恢复时才发现"。往上挪一秒即可：
// 时间戳偏差一两秒看不出来，换来的是"链上每个包都还在"。
func uniqueBackupFileName(bdir string, ts int64, name string) (string, error) {
	for i := 0; i < 120; i++ {
		cand := fmt.Sprintf("bk_%d_%s.tar.gz", ts+int64(i), name)
		if _, err := os.Stat(filepath.Join(bdir, cand)); os.IsNotExist(err) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("备份目录里同名备份过多，无法为 %q 生成文件名", name)
}

// changedSince 根据上一环清单与本轮扫描结果，算出「需要进本次增量」的文件与「已删除」的路径。
//
// 判据（与 DESIGN-INCREMENTAL-BACKUP.md §3.2 一致）：
//   - 上一环没有 → 新文件，进增量
//   - size 或 mtime 不同 → 改过，进增量
//   - size 与 mtime 都相同 → 跳过（**不重算哈希**，否则每次都要全量读盘，增量就没收益）
//   - 上一环有、本轮没有 → 记墓碑
//
// prev 为 nil 表示没有可用的上一环：此时所有文件都算「新」，等同一个完整基线。
func changedSince(prev *backupManifest, current []manifestFile) (changed []manifestFile, deleted []string) {
	if prev == nil {
		return current, nil
	}
	type sig struct{ size, mtime int64 }
	prevSig := make(map[string]sig, len(prev.Files))
	for _, f := range prev.Files {
		prevSig[f.Path] = sig{f.Size, f.Mtime}
	}
	seen := make(map[string]bool, len(current))
	for _, f := range current {
		seen[f.Path] = true
		p, ok := prevSig[f.Path]
		if !ok {
			changed = append(changed, f) // 新增
			continue
		}
		if p.size != f.Size || p.mtime != f.Mtime {
			changed = append(changed, f) // 改过
		}
	}
	for _, f := range prev.Files {
		if !seen[f.Path] {
			deleted = append(deleted, f.Path) // 删除
		}
	}
	sort.Strings(deleted)
	return changed, deleted
}

// latestChainManifest 在备份目录里找**链上最新**的一份清单（只认自动备份，手动备份不入链）。
//
// 为什么只认 auto：手动备份是独立完整的保险，它随时可能被用户删掉。若把它当基准，
// 用户删一次手动备份就会让整条增量链断掉。
//
// 名字与时间戳都从**同一个文件名**解析（parseBackupName）：以前的写法用
// strings.Contains(n, "_auto") 认链、再用 LastIndex("_") 取时间戳，既会把
// "我的auto备份" 这种手动名当成链头，也会在名字里带下划线时解析出错误时间戳。
func (s *Server) latestChainManifest(bdir string) (*backupManifest, string) {
	ents, err := os.ReadDir(bdir)
	if err != nil {
		return nil, ""
	}
	var best *backupManifest
	var bestName string
	var bestTS int64 = -1
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".tar.gz.json") {
			continue
		}
		// 清单名 = 包名 + ".json"，所以去掉 .json 就是链上环的标识（也是 prev_id 的写法）
		tarName := strings.TrimSuffix(n, ".json")
		name, ts := parseBackupName(tarName, time.Time{})
		if !isAutoBackupName(name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(bdir, n))
		if err != nil {
			continue
		}
		var m backupManifest
		if json.Unmarshal(raw, &m) != nil || m.Schema != 1 {
			continue
		}
		// 文件名里的时间戳是排序依据（比 mtime 可靠：拷贝/解压后 mtime 会变）
		if ts > bestTS {
			bestTS, best, bestName = ts, &m, tarName
		}
	}
	return best, bestName
}

// chainState 一条增量链的形状：环数（含基线）、基线包名、基线建立时间。
type chainState struct {
	Depth  int
	BaseID string
	BaseAt time.Time
}

// chainState 从**最新一环**往回走到基线。
//
// 为什么必须走到底：基线的名字与建立时间不在下一环里，只能顺着 prev_id 找；
// 顺手也验证了"链完整" —— 任一环的清单或**包本身**不见了就返回 ok=false，
// 让调用方重建基线（DESIGN §3.4）。少一环的链看着能恢复，真恢复时才会报错，
// 那时候用户已经把新的自动备份当成保险了。
func (s *Server) chainState(bdir string, m *backupManifest, name string) (chainState, bool) {
	cur, curName := m, name
	for depth := 1; depth <= maxChainWalk && cur != nil; depth++ {
		if _, err := os.Stat(filepath.Join(bdir, curName)); err != nil {
			return chainState{}, false // 只剩清单，包被删了/写到一半
		}
		if cur.PrevID == "" {
			return chainState{Depth: depth, BaseID: curName, BaseAt: cur.CreatedAt}, true
		}
		raw, err := os.ReadFile(filepath.Join(bdir, cur.PrevID+".json"))
		if err != nil {
			return chainState{}, false
		}
		var p backupManifest
		if json.Unmarshal(raw, &p) != nil {
			return chainState{}, false
		}
		cur, curName = &p, cur.PrevID
	}
	return chainState{}, false
}

// chainDecision 本轮备份相对增量链的结论。
type chainDecision struct {
	Kind    string                // base | inc
	BaseID  string                // inc：链上基线包名；base：它自己
	PrevID  string                // inc：上一环包名；base：空（新链的起点）
	Include func(rel string) bool // inc：只打包变化的文件；base：nil（全量）
	Deleted []string              // 墓碑：上一环有、本轮没有
	Packed  int                   // 判据认为"变了"的文件数（日志用）
	Why     string                // 写新基线的理由（日志用）
}

// decideChain 判断本轮备份是"接在链尾的增量"还是"新基线"。
//
// 只有**自动**备份入链：手动备份永远是完整快照、且不进入任何链
// （DESIGN §5 —— 手动备份是出事时的最后一道保险，它不能依赖任何其它文件）。
func (s *Server) decideChain(bdir, fileName string, isAuto, scanOK bool, current []manifestFile,
	includeConfig bool, now time.Time) chainDecision {
	d := chainDecision{Kind: "base", BaseID: fileName}
	switch {
	case !isAuto:
		d.Why = "手动备份永远是完整快照，不入链"
		return d
	case !scanOK:
		d.Why = "目录扫描失败，无法判定变化"
		return d
	}

	prev, prevName := s.latestChainManifest(bdir)
	if prev == nil {
		d.Why = "还没有可用的自动备份"
		return d
	}
	st, ok := s.chainState(bdir, prev, prevName)
	switch {
	case !ok:
		d.Why = "链不完整（清单或包缺失）"
	case st.Depth >= maxChainDepth:
		d.Why = fmt.Sprintf("链已有 %d 环（上限 %d）", st.Depth, maxChainDepth)
	case st.BaseAt.IsZero() || now.Sub(st.BaseAt) > maxChainAge:
		d.Why = "基线已超过 7 天"
	case prev.IncludeConfig == nil || *prev.IncludeConfig != includeConfig:
		// 备份**范围**变了就必须重建基线：上一环的清单里有（或没有）配置项，
		// 拿它做对比会把"这次没扫到"误判成"已经删除"，而墓碑是会被真的执行的。
		d.Why = "备份范围与上一环不同（include_config 变化）"
	default:
		changed, deleted := changedSince(prev, current)
		set := make(map[string]bool, len(changed))
		for _, f := range changed {
			set[f.Path] = true
		}
		d.Kind = "inc"
		d.BaseID = st.BaseID
		d.PrevID = prevName
		d.Deleted = deleted
		d.Packed = len(changed)
		d.Include = func(rel string) bool { return set[rel] }
	}
	return d
}

// excludedTopLevel 判断某个顶层条目是否被备份规则排除。
//
// 抽成函数的原因：扫描（算增量）与打包（写 tar）**必须用同一套规则**。
// 两处各写一份的后果是规则漂移 —— 扫描时包含、打包时排除，
// 于是清单里记了文件、包里却没有，下一轮被判「已删除」，恢复时真的删掉。
func excludedTopLevel(rel string, info os.FileInfo, includeConfig bool) bool {
	top := strings.SplitN(rel, "/", 2)[0]
	if skipDirs[top] {
		return true
	}
	if !includeConfig && configEntries[top] {
		return true
	}
	if !includeConfig && info.Name() == "session.lock" {
		return true
	}
	// 符号链接等非常规文件一律跳过
	return !info.Mode().IsRegular() && !info.IsDir()
}

// scanEntries 只做 stat 遍历，返回本轮**完整**的文件清单（不读内容、不写包）。
//
// 增量备份需要「先知道本轮有哪些文件、与上一轮差在哪」才能决定打包什么，
// 所以这一步必须在打包之前跑一次。它只调用 Walk + stat，不读文件内容。
func scanEntries(root string, includeConfig bool) ([]manifestFile, error) {
	var entries []manifestFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if excludedTopLevel(rel, info, includeConfig) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode().IsRegular() {
			entries = append(entries, manifestFile{Path: rel, Size: info.Size(), Mtime: info.ModTime().Unix()})
		}
		return nil
	})
	return entries, err
}
