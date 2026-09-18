package grpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/fileops"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/jobqueue"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// downloadChunkSize 下载分片大小。1 MB 是吞吐与内存的折中：
// 太小会让 gRPC 调用次数暴涨，太大则单个实例的下载会占住大量缓冲。
const downloadChunkSize = 1 << 20

// searchHardLimit 搜索时遍历的最大目录项数，防止在超大实例目录上
// 卡住 Daemon 的工作线程（搜索是同步 RPC，必须尽快返回）。
const searchHardLimit = 200000

// ---- 重命名 ----

// RenameFile 在同一目录内重命名文件或目录。
func (s *Server) RenameFile(ctx context.Context, req *pb.RenameFileRequest) (*pb.OperationResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	if isProtectedPath(req.Path) {
		return &pb.OperationResponse{Success: false, Error: errProtected}, nil
	}
	name := strings.TrimSpace(req.NewName)
	if name == "" {
		return &pb.OperationResponse{Success: false, Error: "新名称不能为空"}, nil
	}
	// 只接受纯文件名：新名字里带路径分隔符就成了"移动"，
	// 会绕过「目标是否可写」等检查，语义也不同
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return &pb.OperationResponse{Success: false, Error: "新名称不能包含路径分隔符"}, nil
	}

	src, err := resolvePath(dir, req.Path)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	if _, err := os.Stat(src); err != nil {
		return &pb.OperationResponse{Success: false, Error: "原文件不存在"}, nil
	}
	dst := filepath.Join(filepath.Dir(src), name)
	if src == dst {
		return &pb.OperationResponse{Success: true, Message: "名称未改变"}, nil
	}
	if _, err := os.Stat(dst); err == nil {
		return &pb.OperationResponse{Success: false, Error: "同名文件已存在"}, nil
	}

	// 改名核心 jar 会让实例下次启动失败（registry 里记的是旧路径），
	// 与删除同理，先行拦下并提示用「核心管理」切换。
	if inst, ok := s.reg.Get(req.InstanceId); ok && samePath(inst.JarPath, src) {
		return &pb.OperationResponse{Success: false,
			Error: "该文件是实例当前使用的核心 jar，请先在「核心」页切换后再重命名"}, nil
	}

	if err := os.Rename(src, dst); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	// 交给实例的运行用户（改名不改属主，但**移动**会跟着父目录变化，
	// 而这一步同时兜住了"原本是 root 写的文件被改到别处"的情况）
	s.handOver(req.InstanceId, dst)
	s.log.Info("文件已重命名", "instance", req.InstanceId, "from", req.Path, "to", name)
	return &pb.OperationResponse{Success: true, Message: "重命名成功"}, nil
}

// ---- 复制 / 移动 ----

// CopyFile 复制或移动文件/目录。
//
// 这里走同步接口而非任务队列：面板侧只对本目录内、体积可控的对象调用它
// （大目录由前端引导到「压缩 / 解压」等排队任务）。真正的重活仍由
// files 接口的调用方决定是否排队。
func (s *Server) CopyFile(ctx context.Context, req *pb.CopyFileRequest) (*pb.OperationResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	// 源与目标都在受保护名单里时一律拒绝：否则可以先复制出 frpc.toml
	// 再改名绕过保护
	if isProtectedPath(req.Src) {
		return &pb.OperationResponse{Success: false, Error: errProtected}, nil
	}
	if isProtectedPath(req.Dst) {
		return &pb.OperationResponse{Success: false, Error: "目标路径与面板内部文件冲突"}, nil
	}
	if req.Src == "" || req.Dst == "" {
		return &pb.OperationResponse{Success: false, Error: "源路径与目标路径均不能为空"}, nil
	}
	if req.Src == req.Dst {
		return &pb.OperationResponse{Success: false, Error: "源与目标是同一个路径"}, nil
	}

	src, err := resolvePath(dir, req.Src)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	dst, err := resolvePath(dir, req.Dst)
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	if _, err := os.Stat(src); err != nil {
		return &pb.OperationResponse{Success: false, Error: "源文件不存在"}, nil
	}
	// 禁止把目录移到自身内部（会无限递归/丢失数据）
	if isSubPath(src, dst) {
		return &pb.OperationResponse{Success: false, Error: "不能把目录移动或复制到它自己的子目录中"}, nil
	}
	if inst, ok := s.reg.Get(req.InstanceId); ok && samePath(inst.JarPath, src) && req.Move {
		return &pb.OperationResponse{Success: false,
			Error: "该文件是实例当前使用的核心 jar，请先在「核心」页切换后再移动"}, nil
	}

	action := "复制"
	if req.Move {
		action = "移动"
	}
	var runErr error
	if req.Move {
		runErr = fileops.Move(ctx, src, dst, req.Overwrite, nil)
	} else {
		runErr = fileops.Copy(ctx, src, dst, req.Overwrite, nil)
	}
	if runErr != nil {
		return &pb.OperationResponse{Success: false, Error: runErr.Error()}, nil
	}
	// 复制/移动的产物都要交给实例用户：移动时源可能是 root 建的，
	// 复制时目标整个是新的（可能是整棵子树）
	s.handOver(req.InstanceId, dst)
	s.log.Info("文件已"+action, "instance", req.InstanceId, "from", req.Src, "to", req.Dst)
	return &pb.OperationResponse{Success: true, Message: action + "成功"}, nil
}

// ---- 搜索 ----

// SearchFiles 在目录树中按名称关键字搜索（大小写不敏感）。
func (s *Server) SearchFiles(ctx context.Context, req *pb.SearchFilesRequest) (*pb.ListFilesResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.ListFilesResponse{Success: false, Error: err.Error()}, nil
	}
	root, err := resolvePath(dir, req.Path)
	if err != nil {
		return &pb.ListFilesResponse{Success: false, Error: err.Error()}, nil
	}
	kw := strings.ToLower(strings.TrimSpace(req.Keyword))
	if kw == "" {
		return &pb.ListFilesResponse{Success: false, Error: "请输入搜索关键字"}, nil
	}
	limit := int(req.Limit)
	if limit <= 0 {
		limit = 200
	}

	files := []*pb.FileInfo{}
	visited := 0
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 无权限/已删除的条目跳过即可
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		visited++
		if visited > searchHardLimit {
			return fs.SkipAll
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if isProtectedPath(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// 备份目录里全是压缩包，既多又大，搜出来没有意义
		if d.IsDir() && d.Name() == "backups" {
			return fs.SkipDir
		}
		if !strings.Contains(strings.ToLower(d.Name()), kw) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, &pb.FileInfo{
			Name:    d.Name(),
			Path:    rel,
			IsDir:   d.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
		})
		if len(files) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) && ctx.Err() == nil {
		return &pb.ListFilesResponse{Success: false, Error: walkErr.Error()}, nil
	}

	// 目录在前，其次按路径排序，结果更稳定
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return files[i].Path < files[j].Path
	})
	return &pb.ListFilesResponse{Success: true, Files: files}, nil
}

// ---- 下载 ----

// DownloadFile 流式下发文件内容。
//
// 不复用 ReadFile：那条路径为「文本编辑」设计，有 1 MB 上限且按 string
// 传输（二进制会被破坏）。下载必须按原字节流走。
func (s *Server) DownloadFile(req *pb.DownloadFileRequest, stream pb.DaemonService_DownloadFileServer) error {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return err
	}
	if isProtectedPath(req.Path) {
		return errors.New(errProtected)
	}
	target, err := resolvePath(dir, req.Path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("是目录，无法直接下载。请先压缩成压缩包再下载")
	}

	f, err := os.Open(target)
	if err != nil {
		return err
	}
	defer f.Close()

	total := info.Size()
	buf := make([]byte, downloadChunkSize)
	for {
		if err := stream.Context().Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			// 复制一份：buf 会被下一轮 Read 覆盖，而 gRPC 发送是异步的
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := stream.Send(&pb.FileChunk{Data: chunk, Total: total}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// ---- 排队任务 ----

// SubmitJob 提交一个压缩 / 解压任务。
func (s *Server) SubmitJob(ctx context.Context, req *pb.SubmitJobRequest) (*pb.SubmitJobResponse, error) {
	if s.jobs == nil {
		return &pb.SubmitJobResponse{Success: false, Error: "任务队列未启用"}, nil
	}
	if req.JobId == "" {
		return &pb.SubmitJobResponse{Success: false, Error: "缺少任务 ID"}, nil
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind != jobqueue.KindCompress && kind != jobqueue.KindExtract {
		return &pb.SubmitJobResponse{Success: false, Error: "不支持的任务类型：" + req.Kind}, nil
	}
	// 提交时就校验路径，避免任务排队半天后才因为越权失败
	if _, err := s.instanceDir(req.InstanceId); err != nil {
		return &pb.SubmitJobResponse{Success: false, Error: err.Error()}, nil
	}
	if isProtectedPath(req.Src) || isProtectedPath(req.Dst) {
		return &pb.SubmitJobResponse{Success: false, Error: errProtected}, nil
	}

	job := &jobqueue.Job{
		ID:         req.JobId,
		InstanceID: req.InstanceId,
		Kind:       kind,
		Src:        req.Src,
		Dst:        req.Dst,
		Format:     strings.ToLower(strings.TrimSpace(req.Format)),
	}
	pos, ok := s.jobs.Submit(job)
	if !ok {
		return &pb.SubmitJobResponse{Success: false, Error: "任务队列已停止"}, nil
	}
	s.log.Info("任务已入队", "job", req.JobId, "kind", kind, "instance", req.InstanceId, "position", pos)
	return &pb.SubmitJobResponse{Success: true, JobId: req.JobId, QueuePosition: int32(pos)}, nil
}

// GetJob 查询任务状态。
func (s *Server) GetJob(ctx context.Context, req *pb.JobRequest) (*pb.JobStatus, error) {
	if s.jobs == nil {
		return &pb.JobStatus{JobId: req.JobId, State: "unknown"}, nil
	}
	j, ok := s.jobs.Get(req.JobId)
	if !ok {
		return &pb.JobStatus{JobId: req.JobId, State: "unknown"}, nil
	}
	return &pb.JobStatus{
		JobId:      j.ID,
		InstanceId: j.InstanceID,
		Kind:       j.Kind,
		State:      j.State,
		Progress:   int32(j.Progress),
		Message:    j.Message,
		Error:      j.Error,
		StartedAt:  j.StartedAt,
		FinishedAt: j.FinishedAt,
		TotalBytes: j.TotalBytes,
		DoneBytes:  j.DoneBytes,
	}, nil
}

// CancelJob 取消任务。
func (s *Server) CancelJob(ctx context.Context, req *pb.JobRequest) (*pb.OperationResponse, error) {
	if s.jobs == nil {
		return &pb.OperationResponse{Success: false, Error: "任务队列未启用"}, nil
	}
	state, ok := s.jobs.Cancel(req.JobId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "任务不存在（可能已完成并被清理）"}, nil
	}
	switch state {
	case jobqueue.StateSuccess, jobqueue.StateFailed, jobqueue.StateCanceled:
		return &pb.OperationResponse{Success: false, Error: "任务已结束，无法取消"}, nil
	case jobqueue.StateRunning:
		return &pb.OperationResponse{Success: true, Message: "已请求取消，正在等待当前步骤结束"}, nil
	default:
		return &pb.OperationResponse{Success: true, Message: "任务已取消"}, nil
	}
}

// runJob 是任务队列的执行体（在 worker goroutine 中运行）。
//
// 这里做两件在提交时无法完成的事：
//  1. 把实例相对路径解析为绝对路径（提交时实例可能还没就绪）；
//  2. 构造「受保护文件」过滤器 —— 必须按归档内的相对路径重新映射回
//     实例内的相对路径，否则压缩实例根目录会把 frpc.toml（含 frps 共享
//     密钥）一并打包出去。
func (s *Server) runJob(ctx context.Context, job *jobqueue.Job, report jobqueue.Report) error {
	dir, err := s.instanceDir(job.InstanceID)
	if err != nil {
		return err
	}
	src, err := resolvePath(dir, job.Src)
	if err != nil {
		return err
	}
	dst, err := resolvePath(dir, job.Dst)
	if err != nil {
		return err
	}

	toReport := func(progress int, message string, done, total int64) {
		if report != nil {
			report(progress, message, done, total)
		}
	}

	switch job.Kind {
	case jobqueue.KindCompress:
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("源路径不存在：%s", job.Src)
		}
		baseDir := path.Dir(strings.TrimPrefix(job.Src, "/"))
		filter := func(name string) bool {
			rel := path.Clean(path.Join(baseDir, name))
			return !isProtectedPath(rel)
		}
		if err := fileops.Compress(ctx, src, dst, job.Format, filter, toReport); err != nil {
			return err
		}
		// 产物交给实例用户：备份/压缩包也是租户要自己下载、移动、删除的东西
		s.handOver(job.InstanceID, dst)
		info, err := os.Stat(dst)
		if err == nil {
			toReport(100, fmt.Sprintf("已生成 %s（%s）", filepath.Base(dst), humanSize(info.Size())), info.Size(), info.Size())
		}
		return nil

	case jobqueue.KindExtract:
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("压缩包不存在：%s", job.Src)
		}
		baseDir := strings.TrimPrefix(job.Dst, "/")
		filter := func(name string) bool {
			rel := path.Clean(path.Join(baseDir, name))
			return !isProtectedPath(rel)
		}
		// 用 defer：解压到一半失败时，已经落盘的那半个目录树同样是 root 属主，
		// 不交出去的话用户重试时会撞上"目录里建不了东西"。
		defer s.handOver(job.InstanceID, dst)
		if err := fileops.Extract(ctx, src, dst, job.Format, filter, toReport); err != nil {
			return err
		}
		toReport(100, "解包完成", 0, 0)
		return nil
	}
	return fmt.Errorf("未知任务类型：%s", job.Kind)
}

// ---- 玩家总览 ----

// playerStats 是 world/stats/<uuid>.json 里我们关心的部分。
type playerStats struct {
	Stats struct {
		Custom map[string]int64 `json:"minecraft:custom"`
	} `json:"stats"`
}

type userCacheEntry struct {
	Name string `json:"name"`
	UUID string `json:"uuid"`
}

type profileEntry struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Level   int    `json:"level"`
	Reason  string `json:"reason"`
	Expires string `json:"expires"`
}

// GetPlayerOverview 汇总「所有曾经进过服的玩家」。
//
// 数据来源与取舍：
//   - usercache.json —— 服务器记录过的玩家名↔UUID 映射（最全的历史名册）
//   - 白名单/OP/封禁名单 —— 标记玩家身份
//   - world/stats/<uuid>.json —— 累计游戏时长（唯一可靠的时长来源）
//   - logs/latest.log —— 判断谁**此刻**在线（没有更好的跨核心通用来源）
//
// 之所以要合并多个来源：usercache 只覆盖"服务器见过的人"，
// 而统计文件覆盖"真正在世界里玩过的人"。两者取并集才不遗漏。
func (s *Server) GetPlayerOverview(ctx context.Context, req *pb.InstanceRequest) (*pb.PlayerOverviewResponse, error) {
	dir, err := s.instanceDir(req.InstanceId)
	if err != nil {
		return &pb.PlayerOverviewResponse{Success: false, Error: err.Error()}, nil
	}

	resp := &pb.PlayerOverviewResponse{Success: true}

	// 世界目录名与白名单开关都来自 server.properties；缺省 world
	resp.WorldName = "world"
	if b, err := os.ReadFile(filepath.Join(dir, "server.properties")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "level-name="):
				if v := strings.TrimSpace(strings.TrimPrefix(line, "level-name=")); v != "" {
					resp.WorldName = v
				}
			case strings.HasPrefix(line, "white-list="):
				resp.WhitelistEnabled = strings.HasSuffix(line, "=true")
			}
		}
	}

	type acc struct {
		uuid      string
		name      string
		play      int64
		whitelist bool
		op        bool
		banned    bool
		hasData   bool
		lastSeen  int64
		source    string
	}
	byUUID := map[string]*acc{}
	byName := map[string]*acc{}

	ensure := func(uuid, name string) *acc {
		key := strings.ToLower(uuid)
		if key != "" {
			if a, ok := byUUID[key]; ok {
				if a.name == "" {
					a.name = name
				}
				return a
			}
		}
		if name != "" {
			if a, ok := byName[strings.ToLower(name)]; ok {
				if a.uuid == "" {
					a.uuid = uuid
				}
				return a
			}
		}
		a := &acc{uuid: uuid, name: name}
		if key != "" {
			byUUID[key] = a
		}
		if name != "" {
			byName[strings.ToLower(name)] = a
		}
		return a
	}

	// 1) usercache.json：历史名册
	var cache []userCacheEntry
	if b, err := os.ReadFile(filepath.Join(dir, "usercache.json")); err == nil {
		if json.Unmarshal(b, &cache) == nil {
			for _, e := range cache {
				if e.Name == "" {
					continue
				}
				a := ensure(e.UUID, e.Name)
				if a.source == "" {
					a.source = "usercache"
				}
			}
		}
	}

	// 2) 名单类文件
	readProfiles := func(file string) []profileEntry {
		var list []profileEntry
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return nil
		}
		_ = json.Unmarshal(b, &list)
		return list
	}
	for _, e := range readProfiles("whitelist.json") {
		if e.Name == "" {
			continue
		}
		a := ensure(e.UUID, e.Name)
		a.whitelist = true
		if a.source == "" {
			a.source = "whitelist"
		}
	}
	for _, e := range readProfiles("ops.json") {
		if e.Name == "" {
			continue
		}
		a := ensure(e.UUID, e.Name)
		a.op = true
		if a.source == "" {
			a.source = "ops"
		}
	}
	for _, e := range readProfiles("banned-players.json") {
		if e.Name == "" {
			continue
		}
		a := ensure(e.UUID, e.Name)
		a.banned = true
		if a.source == "" {
			a.source = "bans"
		}
	}

	// 3) world/stats/<uuid>.json：游戏时长 + 是否真正进过世界
	statsDir := filepath.Join(dir, resp.WorldName, "stats")
	if entries, err := os.ReadDir(statsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			uuid := strings.TrimSuffix(e.Name(), ".json")
			b, err := os.ReadFile(filepath.Join(statsDir, e.Name()))
			if err != nil {
				continue
			}
			var ps playerStats
			if json.Unmarshal(b, &ps) != nil {
				continue
			}
			ticks := ps.Stats.Custom["minecraft:play_time"]
			if ticks == 0 {
				// 1.12 及更早版本用 play_one_minute（单位是 tick，不是分钟）
				ticks = ps.Stats.Custom["minecraft:play_one_minute"]
			}
			a := ensure(uuid, "")
			a.hasData = true
			// play_time 单位是 tick（20 tick/秒）
			a.play = ticks / 20
			if info, err := e.Info(); err == nil {
				a.lastSeen = info.ModTime().Unix()
			}
			if a.source == "" {
				a.source = "stats"
			}
		}
	}

	// 4) 在线玩家（从最新日志推断）
	online := s.onlinePlayers(dir)
	for name := range online {
		a := ensure("", name)
		a.name = name
	}

	list := make([]*pb.PlayerOverviewEntry, 0, len(byUUID)+len(byName))
	seen := map[*acc]bool{}
	for _, a := range byUUID {
		if seen[a] || (a.name == "" && !a.hasData) {
			continue
		}
		seen[a] = true
		list = append(list, toEntry(a.name, a.uuid, a.play, a.whitelist, a.op, a.banned, a.hasData, a.lastSeen, a.source, online))
	}
	for _, a := range byName {
		if seen[a] || (a.name == "" && !a.hasData) {
			continue
		}
		seen[a] = true
		list = append(list, toEntry(a.name, a.uuid, a.play, a.whitelist, a.op, a.banned, a.hasData, a.lastSeen, a.source, online))
	}

	// 在线优先，其次按游戏时长（没有时长的排最后）
	sort.Slice(list, func(i, j int) bool {
		if list[i].Online != list[j].Online {
			return list[i].Online
		}
		return list[i].PlaySeconds > list[j].PlaySeconds
	})

	for _, e := range list {
		if e.Online {
			resp.Online++
		}
	}
	resp.Total = int32(len(list))
	resp.Players = list
	return resp, nil
}

func toEntry(name, uuid string, play int64, wl, op, banned, hasData bool, lastSeen int64, source string, online map[string]bool) *pb.PlayerOverviewEntry {
	return &pb.PlayerOverviewEntry{
		Uuid:        uuid,
		Name:        name,
		PlaySeconds: play,
		Online:      online[strings.ToLower(name)],
		Whitelisted: wl,
		Op:          op,
		Banned:      banned,
		HasData:     hasData,
		LastSeen:    lastSeen,
		Source:      source,
	}
}

// onlinePlayers 从 logs/latest.log 推断当前在线玩家。
//
// 做法：从最后一次「服务器启动完成」标记之后开始统计加入/退出事件。
// 日志里没有直接可用的在线名单（各核心的查询命令格式不一），
// 这是跨核心（原版 / Paper / Folia / 各类服务端）最通用的一种推断方式。
func (s *Server) onlinePlayers(dir string) map[string]bool {
	out := map[string]bool{}
	logPath := filepath.Join(dir, "logs", "latest.log")
	f, err := os.Open(logPath)
	if err != nil {
		return out
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return out
	}
	const maxRead = int64(64) << 20
	var data []byte
	if info.Size() > maxRead {
		// 超大日志只读末尾：服务器启动标记通常也在最近一段内
		if _, err := f.Seek(info.Size()-maxRead, io.SeekStart); err != nil {
			return out
		}
		data, _ = io.ReadAll(f)
	} else {
		data, _ = io.ReadAll(f)
	}

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		idx := strings.LastIndex(line, "]: ")
		if idx < 0 {
			continue
		}
		msg := strings.TrimSpace(line[idx+3:])

		// 服务器成功启动 → 前面的加入/退出记录都属于上一轮，清空重算
		if strings.HasPrefix(msg, "Done (") {
			for k := range out {
				delete(out, k)
			}
			continue
		}
		if name, ok := trimSuffixCI(msg, " joined the game"); ok {
			out[strings.ToLower(name)] = true
			continue
		}
		if name, ok := trimSuffixCI(msg, " left the game"); ok {
			delete(out, strings.ToLower(name))
			continue
		}
		// 部分核心的离线提示（超时/连接中断）同样意味着玩家已不在线
		if name, ok := trimSuffixCI(msg, " lost connection"); ok {
			delete(out, strings.ToLower(name))
		}
	}
	return out
}

// trimSuffixCI 大小写不敏感地判断并剥离后缀，返回前缀（玩家名）与是否匹配。
// 玩家名里不会出现空格后缀冲突，因此直接按后缀匹配是安全的。
func trimSuffixCI(s, suffix string) (string, bool) {
	if len(s) <= len(suffix) {
		return "", false
	}
	if !strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return "", false
	}
	return strings.TrimSpace(s[:len(s)-len(suffix)]), true
}

// ---- 实例指令 ----

// SendCommand 向运行中的实例下发一条控制台指令（供定时任务使用）。
func (s *Server) SendCommand(ctx context.Context, req *pb.CommandRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.Command), "/"))
	if cmd == "" {
		return &pb.OperationResponse{Success: false, Error: "指令不能为空"}, nil
	}
	if inst.Status() != "running" {
		return &pb.OperationResponse{Success: false, Error: "实例未在运行，无法下发指令"}, nil
	}
	if err := inst.SendCommand(cmd); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	s.log.Info("已下发实例指令", "instance", req.InstanceId, "command", cmd)
	return &pb.OperationResponse{Success: true, Message: "指令已下发"}, nil
}

// ---- 小工具 ----

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// isSubPath 判断 child 是否位于 parent 之内（含相等）。
func isSubPath(parent, child string) bool {
	p, _ := filepath.Abs(parent)
	c, _ := filepath.Abs(child)
	if p == c {
		return true
	}
	return strings.HasPrefix(c, p+string(filepath.Separator))
}
