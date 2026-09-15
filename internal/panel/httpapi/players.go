package httpapi

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 玩家名单类型与对应的数据文件
var playerFiles = map[string]string{
	"whitelist": "whitelist.json",
	"ops":       "ops.json",
	"bans":      "banned-players.json",
	"ipbans":    "banned-ips.json",
}

// playerEntry 统一的玩家条目视图。
type playerEntry struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Level   int    `json:"level,omitempty"`   // ops 专用
	Reason  string `json:"reason,omitempty"`  // bans 专用
	Source  string `json:"source,omitempty"`  // bans 专用
	Expires string `json:"expires,omitempty"` // bans 专用
	IP      string `json:"ip,omitempty"`      // ipbans 专用
}

// handlePlayerOverview GET /api/instances/{id}/players/all
//
// 「全部玩家信息」：把服务器见过的所有玩家汇总成一张表 ——
// 累计游戏时长、是否在线、白名单/OP/封禁状态。
//
// 与上面几个名单接口的区别：那些是**逐个名单文件**的编辑视图，
// 这里是**跨文件合并**的只读总览，回答"这个服一共来过谁、谁玩得最多"。
func (s *Server) handlePlayerOverview(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	// 要读 usercache + 全部 stats 文件 + 扫日志，给足超时
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := cli.GetPlayerOverview(ctx, &pb.InstanceRequest{InstanceId: instanceID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}

	type overviewEntry struct {
		UUID        string `json:"uuid"`
		Name        string `json:"name"`
		PlaySeconds int64  `json:"play_seconds"`
		Online      bool   `json:"online"`
		Whitelisted bool   `json:"whitelisted"`
		Op          bool   `json:"op"`
		Banned      bool   `json:"banned"`
		HasData     bool   `json:"has_data"`
		LastSeen    int64  `json:"last_seen"`
		Source      string `json:"source"`
	}
	list := make([]overviewEntry, 0, len(resp.Players))
	for _, p := range resp.Players {
		list = append(list, overviewEntry{
			UUID: p.Uuid, Name: p.Name, PlaySeconds: p.PlaySeconds,
			Online: p.Online, Whitelisted: p.Whitelisted, Op: p.Op,
			Banned: p.Banned, HasData: p.HasData, LastSeen: p.LastSeen, Source: p.Source,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"players":           list,
		"total":             resp.Total,
		"online":            resp.Online,
		"whitelist_enabled": resp.WhitelistEnabled,
		"world_name":        resp.WorldName,
	})
}

// readPlayerFile 读取名单文件；文件不存在视为空名单。
func (s *Server) readPlayerFile(cli pb.DaemonServiceClient, instanceID, file string) (string, bool, error) {
	resp, err := cli.ReadFile(context.Background(), &pb.ReadFileRequest{InstanceId: instanceID, Path: file})
	if err != nil {
		return "", false, err
	}
	if !resp.Success {
		// 文件不存在是正常情况（例如从未配置过白名单）
		return "", false, nil
	}
	return resp.Content, true, nil
}

// handleListPlayers GET /api/instances/{id}/players?type=whitelist|ops|bans|ipbans
func (s *Server) handleListPlayers(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	kind := r.URL.Query().Get("type")
	if kind == "" {
		kind = "whitelist"
	}
	file, ok := playerFiles[kind]
	if !ok {
		writeErr(w, http.StatusBadRequest, "未知名单类型")
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	content, exists, err := s.readPlayerFile(cli, instanceID, file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	entries := parsePlayerEntries(kind, content)
	// 白名单总开关（server.properties 中的 white-list）
	whitelistEnabled := false
	if kind == "whitelist" {
		whitelistEnabled = s.readWhitelistSwitch(cli, instanceID)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"type":              kind,
		"file":              file,
		"exists":            exists,
		"entries":           entries,
		"whitelist_enabled": whitelistEnabled,
	})
}

// handleAddPlayer POST /api/instances/{id}/players
// body: {"type":"whitelist","name":"Steve"}
func (s *Server) handleAddPlayer(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "玩家名不能为空")
		return
	}
	if _, ok := playerFiles[req.Type]; !ok {
		writeErr(w, http.StatusBadRequest, "未知名单类型")
		return
	}

	msg, mode, err := s.mutatePlayer(instanceID, req.Type, req.Name, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "add_"+req.Type, instanceID, req.Name+" ("+mode+")")
	writeJSON(w, http.StatusOK, map[string]string{"message": msg, "mode": mode})
}

// handleRemovePlayer DELETE /api/instances/{id}/players?type=&name=
func (s *Server) handleRemovePlayer(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	kind := r.URL.Query().Get("type")
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少玩家名")
		return
	}
	if _, ok := playerFiles[kind]; !ok {
		writeErr(w, http.StatusBadRequest, "未知名单类型")
		return
	}

	msg, mode, err := s.mutatePlayer(instanceID, kind, name, false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "remove_"+kind, instanceID, name+" ("+mode+")")
	writeJSON(w, http.StatusOK, map[string]string{"message": msg, "mode": mode})
}

// mutatePlayer 增删名单成员。
//
// 优先通过控制台命令执行：Minecraft 会自行解析玩家名到 UUID，
// 因此在线模式与离线模式都能正确处理，且无需重启服务器。
// 实例未运行（或无 stdin）时退化为直接编辑 JSON 文件 —— 此时只能按
// 离线模式的算法推导 UUID，响应中会明确标注。
func (s *Server) mutatePlayer(instanceID, kind, name string, add bool) (msg, mode string, err error) {
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		return "", "", fmt.Errorf("实例不存在")
	}

	// 判断实例是否可接收控制台命令
	st, err := cli.GetInstanceStatus(context.Background(), &pb.InstanceRequest{InstanceId: instanceID})
	running := err == nil && st.Status == "running"

	if running {
		cmd, cerr := playerCommand(kind, name, add)
		if cerr == nil {
			if out, serr := s.runConsoleCommand(cli, instanceID, cmd); serr == nil {
				return fmt.Sprintf("已通过控制台执行：%s（%s）", cmd, out), "console", nil
			}
			// 接管态实例无 stdin，会走到下面的文件方式
		}
	}

	// 文件方式
	file := playerFiles[kind]
	content, _, rerr := s.readPlayerFile(cli, instanceID, file)
	if rerr != nil {
		return "", "", rerr
	}
	updated, changed, uerr := updatePlayerJSON(kind, content, name, add)
	if uerr != nil {
		return "", "", uerr
	}
	if !changed {
		if add {
			return fmt.Sprintf("%s 已在名单中", name), "file", nil
		}
		return fmt.Sprintf("%s 不在名单中", name), "file", nil
	}
	wresp, werr := cli.WriteFile(context.Background(), &pb.WriteFileRequest{
		InstanceId: instanceID, Path: file, Content: updated,
	})
	if werr != nil {
		return "", "", werr
	}
	if !wresp.Success {
		return "", "", fmt.Errorf("%s", wresp.Error)
	}

	note := "已直接修改 " + file
	if !running {
		note += "（实例未运行，UUID 按离线模式推导；在线模式服务器请启动后操作或重启生效）"
	} else {
		note += "（实例处于接管状态无法发送命令，需重启实例或重启服务器使改动生效）"
	}
	action := "添加"
	if !add {
		action = "移除"
	}
	return fmt.Sprintf("%s %s 完成：%s", action, name, note), "file", nil
}

// playerCommand 返回对应的控制台命令。
func playerCommand(kind, name string, add bool) (string, error) {
	switch kind {
	case "whitelist":
		if add {
			return "whitelist add " + name, nil
		}
		return "whitelist remove " + name, nil
	case "ops":
		if add {
			return "op " + name, nil
		}
		return "deop " + name, nil
	case "bans":
		if add {
			return "ban " + name, nil
		}
		return "pardon " + name, nil
	case "ipbans":
		if add {
			return "ban-ip " + name, nil
		}
		return "pardon-ip " + name, nil
	}
	return "", fmt.Errorf("未知名单类型")
}

// parsePlayerEntries 解析名单 JSON 为统一视图。
func parsePlayerEntries(kind, content string) []playerEntry {
	out := []playerEntry{}
	if strings.TrimSpace(content) == "" {
		return out
	}
	switch kind {
	case "whitelist":
		var raw []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(content), &raw) != nil {
			return out
		}
		for _, e := range raw {
			out = append(out, playerEntry{UUID: e.UUID, Name: e.Name})
		}
	case "ops":
		var raw []struct {
			UUID                string `json:"uuid"`
			Name                string `json:"name"`
			Level               int    `json:"level"`
			BypassesPlayerLimit bool   `json:"bypassesPlayerLimit"`
		}
		if json.Unmarshal([]byte(content), &raw) != nil {
			return out
		}
		for _, e := range raw {
			out = append(out, playerEntry{UUID: e.UUID, Name: e.Name, Level: e.Level})
		}
	case "bans":
		var raw []struct {
			UUID    string `json:"uuid"`
			Name    string `json:"name"`
			Reason  string `json:"reason"`
			Source  string `json:"source"`
			Expires string `json:"expires"`
		}
		if json.Unmarshal([]byte(content), &raw) != nil {
			return out
		}
		for _, e := range raw {
			out = append(out, playerEntry{UUID: e.UUID, Name: e.Name, Reason: e.Reason, Source: e.Source, Expires: e.Expires})
		}
	case "ipbans":
		var raw []struct {
			IP      string `json:"ip"`
			Reason  string `json:"reason"`
			Source  string `json:"source"`
			Expires string `json:"expires"`
		}
		if json.Unmarshal([]byte(content), &raw) != nil {
			return out
		}
		for _, e := range raw {
			out = append(out, playerEntry{IP: e.IP, Reason: e.Reason, Source: e.Source, Expires: e.Expires})
		}
	}
	return out
}

// updatePlayerJSON 在名单 JSON 中增删成员（返回新内容与是否有变化）。
func updatePlayerJSON(kind, content, name string, add bool) (string, bool, error) {
	entries := parsePlayerEntries(kind, content)
	lower := strings.ToLower(name)

	idx := -1
	for i, e := range entries {
		if e.Name != "" && strings.ToLower(e.Name) == lower {
			idx = i
			break
		}
		if e.IP != "" && e.IP == name {
			idx = i
			break
		}
	}

	if add {
		if idx >= 0 {
			return content, false, nil
		}
		switch kind {
		case "whitelist":
			entries = append(entries, playerEntry{UUID: offlineUUID(name), Name: name})
		case "ops":
			entries = append(entries, playerEntry{UUID: offlineUUID(name), Name: name, Level: 4})
		case "bans":
			entries = append(entries, playerEntry{
				UUID: offlineUUID(name), Name: name,
				Reason: "Banned by an operator.", Source: "ATL-MCPanel",
				Expires: "forever",
			})
		case "ipbans":
			entries = append(entries, playerEntry{
				IP: name, Reason: "Banned by an operator.",
				Source: "ATL-MCPanel", Expires: "forever",
			})
		}
	} else {
		if idx < 0 {
			return content, false, nil
		}
		entries = append(entries[:idx], entries[idx+1:]...)
	}

	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return "", false, err
	}
	return string(b) + "\n", true, nil
}

// runConsoleCommand 通过控制台流发送一条命令，并等待其输出（用于确认执行结果）。
// 控制台命令没有独立 RPC，需借助双向流实现。
func (s *Server) runConsoleCommand(cli pb.DaemonServiceClient, instanceID, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := cli.Console(ctx)
	if err != nil {
		return "", err
	}
	if err := stream.Send(&pb.ConsoleFrame{Type: pb.ConsoleFrame_ATTACH, InstanceId: instanceID}); err != nil {
		return "", err
	}
	// 跳过附加时的历史回放与状态帧，等待 attached 后再发命令
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()

	// 给 Daemon 一点时间完成附加与历史回放
	time.Sleep(300 * time.Millisecond)
	if err := stream.Send(&pb.ConsoleFrame{Type: pb.ConsoleFrame_COMMAND, InstanceId: instanceID, Data: cmd}); err != nil {
		return "", err
	}
	_ = stream.CloseSend()
	return "已发送", nil
}

// offlineUUID 按 Minecraft 离线模式算法推导 UUID（与服务端 offline-mode 行为一致）。
// 在线模式下的真实 UUID 需由服务端查询 Mojang，因此优先使用控制台命令。
func offlineUUID(name string) string {
	h := md5.Sum([]byte("OfflinePlayer:" + name))
	h[6] = (h[6] & 0x0f) | 0x30 // 版本 3
	h[8] = (h[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// readWhitelistSwitch 读取 server.properties 中的 white-list 开关。
func (s *Server) readWhitelistSwitch(cli pb.DaemonServiceClient, instanceID string) bool {
	resp, err := cli.ReadFile(context.Background(), &pb.ReadFileRequest{InstanceId: instanceID, Path: "server.properties"})
	if err != nil || !resp.Success {
		return false
	}
	for _, line := range strings.Split(resp.Content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "white-list=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "white-list=")) == "true"
		}
	}
	return false
}

// handleSetWhitelistSwitch POST /api/instances/{id}/whitelist
// 切换 server.properties 中的 white-list 与 enforce-whitelist。
func (s *Server) handleSetWhitelistSwitch(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	resp, err := cli.ReadFile(context.Background(), &pb.ReadFileRequest{InstanceId: instanceID, Path: "server.properties"})
	if err != nil || !resp.Success {
		writeErr(w, http.StatusNotFound, "实例尚无 server.properties（请先启动一次以生成）")
		return
	}

	val := "false"
	if req.Enabled {
		val = "true"
	}
	lines := strings.Split(resp.Content, "\n")
	seenWL, seenEW := false, false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "white-list=") {
			lines[i] = "white-list=" + val
			seenWL = true
		}
		if strings.HasPrefix(trimmed, "enforce-whitelist=") {
			lines[i] = "enforce-whitelist=" + val
			seenEW = true
		}
	}
	if !seenWL {
		lines = append(lines, "white-list="+val)
	}
	if !seenEW {
		lines = append(lines, "enforce-whitelist="+val)
	}

	wresp, err := cli.WriteFile(context.Background(), &pb.WriteFileRequest{
		InstanceId: instanceID, Path: "server.properties", Content: strings.Join(lines, "\n"),
	})
	if err != nil || !wresp.Success {
		writeErr(w, http.StatusInternalServerError, "写入 server.properties 失败")
		return
	}

	s.audit(r, "set_whitelist", instanceID, val)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "白名单已" + map[bool]string{true: "启用", false: "关闭"}[req.Enabled] + "（重启服务器后生效）",
		"enabled": req.Enabled,
		"note":    "若服务器正在运行，也可通过控制台执行 whitelist on/off 立即生效",
		"time":    time.Now().Format(time.RFC3339),
	})
}
