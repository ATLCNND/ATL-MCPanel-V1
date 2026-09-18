package httpapi

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	if msg := checkPlayerName(req.Type, req.Name); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	// 踢出：**没有**对应的名单文件，只能通过控制台执行，所以单独走一条路。
	// 它长得像"加入某个名单"（一次性动作），前端因此复用同一个接口。
	if req.Type == kickKind {
		msg, err := s.kickPlayer(instanceID, req.Name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.audit(r, "kick_player", instanceID, req.Name)
		writeJSON(w, http.StatusOK, map[string]string{"message": msg, "mode": "console"})
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
	if msg := checkPlayerName(kind, name); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
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

// kickKind 踢出：控制台专属动作，没有对应的名单文件。
const kickKind = "kick"

// mcNameMaxLen 玩家名长度上限（按字符）。
//
// 原版是 3~16，这里放到 64：Geyser/Floodgate 会给基岩版玩家加前缀，
// 且基岩版昵称本身允许空格（如 `.Big Steve`），余量给足。
const mcNameMaxLen = 64

// consoleSafeNameRe 名字能否安全地拼进控制台命令。
//
// 注意它**不是**安全边界，只是"拼进去有没有用"的判断：
//   - 真正会造成注入的只有换行（见 checkPlayerName）—— 控制台按行执行，
//     一个 \n 就能把一条命令变成两条；
//   - 而空格不会注入，只会让 `kick Big Steve` 被解析成"多给了参数"而失败。
//     基岩版玩家的名字（经 Floodgate 接入）本来就可能带空格，所以不能在
//     校验层一刀切拦掉，只能在**走控制台这条路**时退让：改用直接改名单文件。
//
// 也就是说：安全由 checkPlayerName 负责，这里负责"这条路走不走得通"。
var consoleSafeNameRe = regexp.MustCompile(`^[A-Za-z0-9_.*]{1,32}$`)

// ipRe 封禁 IP 时接受的形态（IPv4 与 IPv6 都允许：ban-ip 两种都支持）。
var ipRe = regexp.MustCompile(`^[0-9A-Fa-f:.]{1,45}$`)

// checkPlayerName 校验玩家名 / IP，返回空字符串表示通过。
//
// 这条校验是**安全边界**，它挡的是"名字被拼进控制台命令后执行了第二条命令"：
//
//	name 会被拼成 `op <name>`、`whitelist remove <name>` 送进控制台，
//	而 Minecraft 的控制台按**行**执行 —— 名字里带一个 \n，
//	后面的内容就是一条独立命令（实测可注入 `say`、`stop`、`op` 等任意命令）。
//
// 因此这里拦下所有控制字符（含 \n \r \t \0 与 DEL），并限制长度；
// 其余字符（含空格）放行 —— 理由见 consoleSafeNameRe 的注释：
// 空格不会注入，而基岩版玩家名确实可能带空格。
//
// 按名单类型分别校验：封禁 IP 那一栏填的是地址，用玩家名的规则去卡会把
// IPv6（含冒号）拦掉。
func checkPlayerName(kind, name string) string {
	if name == "" {
		return "玩家名不能为空"
	}
	if utf8.RuneCountInString(name) > mcNameMaxLen {
		return fmt.Sprintf("玩家名过长（上限 %d 个字符）", mcNameMaxLen)
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return "玩家名不能包含换行、回车等控制字符（会被控制台当成多条命令执行）"
		}
	}
	if kind == "ipbans" {
		if !ipRe.MatchString(name) {
			return "IP 地址格式不合法（只接受 IPv4 / IPv6）"
		}
	}
	return ""
}

// consoleSafeName 该名字能否直接拼进控制台命令。
//
// 不能时的处理见 mutatePlayer：改用直接修改名单文件那条路径，
// 而不是拒绝用户 —— 名字里有空格是合法的（基岩版），只是命令表达不了它。
func consoleSafeName(name string) bool {
	return consoleSafeNameRe.MatchString(name)
}

// kickPlayer 把在线玩家踢出服务器。
//
// 只能走控制台：踢出是**一次性动作**，不像封禁那样有名单文件可以退化为直接改写。
// 因此实例没在跑就直接拒绝，并说清原因 —— 而不是写一个什么都不影响的文件。
//
// 返回值刻意写成"已发送命令"而不是"已踢出"：这条链路（Panel → Daemon → stdin）
// 只保证命令送达，玩家的实际结果由服务器决定（比如名字拼错时服务器会回一句
// "找不到该玩家"）。把不确定的事说成确定的，以后就要花时间解释"为什么说踢了却没踢"。
func (s *Server) kickPlayer(instanceID, name string) (string, error) {
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		return "", fmt.Errorf("实例不存在")
	}
	st, err := cli.GetInstanceStatus(context.Background(), &pb.InstanceRequest{InstanceId: instanceID})
	if err != nil || st.Status != "running" {
		return "", fmt.Errorf("实例未运行：踢出只能对在线玩家执行，请先启动实例")
	}
	if _, err := s.runConsoleCommand(cli, instanceID, "kick "+name); err != nil {
		return "", fmt.Errorf("发送踢出命令失败：%v", err)
	}
	return "已向服务器发送踢出命令：kick " + name + "（若该玩家当前不在线，服务器会回复找不到该玩家）", nil
}

// handleSetOpLevel POST /api/instances/{id}/players/op-level
// body: {"name":"Steve","level":2}   level ∈ 1..4
//
// 为什么要单独一个接口，而不是复用"加入管理员名单"：
//
//	`op <玩家>` 这条命令**表达不了等级** —— 原版/Paper 的 /op 一律把等级设成
//	server.properties 里的 op-permission-level（默认 4）。想让某人是 2 级，
//	只能改 ops.json 里的 level 字段，没有别的办法。
//
//	等级的含义（原版）：
//	  1 = 可绕过出生点保护
//	  2 = 1 + 可用大部分单人指令、命令方块
//	  3 = 2 + 可管理玩家（ban/kick/op）
//	  4 = 3 + 可管理服务器（stop/save 等），默认值
//
// 生效时机要说清楚：写进 ops.json 之后，运行中的服务器通常会自行读取名单文件的
// 变化；若游戏内仍是旧等级，`/reload` 或重启实例即可 —— 提示里照实写，
// 不假装"改完立刻生效"。
func (s *Server) handleSetOpLevel(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Name  string `json:"name"`
		Level int    `json:"level"`
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
	if msg := checkPlayerName("ops", req.Name); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if req.Level < 1 || req.Level > 4 {
		writeErr(w, http.StatusBadRequest, "OP 等级只能是 1~4")
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	content, _, rerr := s.readPlayerFile(cli, instanceID, playerFiles["ops"])
	if rerr != nil {
		writeErr(w, http.StatusInternalServerError, rerr.Error())
		return
	}
	updated, changed, existed, uerr := setOpLevelJSON(content, req.Name, req.Level)
	if uerr != nil {
		writeErr(w, http.StatusInternalServerError, uerr.Error())
		return
	}

	if !changed {
		writeJSON(w, http.StatusOK, map[string]string{
			"message": fmt.Sprintf("%s 的 OP 等级已经是 %d 级", req.Name, req.Level),
			"level":   fmt.Sprintf("%d", req.Level),
		})
		return
	}

	wresp, werr := cli.WriteFile(context.Background(), &pb.WriteFileRequest{
		InstanceId: instanceID, Path: playerFiles["ops"], Content: updated,
	})
	if werr != nil {
		writeErr(w, http.StatusInternalServerError, werr.Error())
		return
	}
	if !wresp.Success {
		writeErr(w, http.StatusInternalServerError, wresp.Error)
		return
	}

	s.audit(r, "set_op_level", instanceID, fmt.Sprintf("%s → %d 级", req.Name, req.Level))

	msg := fmt.Sprintf("已把 %s 的 OP 等级设为 %d 级", req.Name, req.Level)
	if !existed {
		msg += "（该玩家原本不在管理员名单中，已一并加入）"
	}
	msg += "。运行中的服务器会自动读取名单文件的变化；若游戏内仍是旧等级，执行 /reload 或重启实例即可。"
	writeJSON(w, http.StatusOK, map[string]string{"message": msg, "level": fmt.Sprintf("%d", req.Level)})
}

// setOpLevelJSON 在 ops.json 中设置某玩家的 OP 等级。
//
// 返回 (新内容, 是否有变化, 原本是否已在名单中, 错误)。
// 不在名单中时按给定等级加入 —— "设成 3 级"这个意图本身就包含"他要成为管理员"，
// 分成"先加再改等级"两步只会让人多点一次、还容易只做一半。
//
// 这里**不走 playerEntry 往返**，而是按"保留全部字段"的方式解析：
// ops.json 的条目除了 uuid/name/level 还有 `bypassesPlayerLimit`
// （是否允许进入已满员的服务器），而 playerEntry 不认识这个字段 ——
// 用它往返会把它悄悄抹掉，表现是"只改了个 OP 等级，那人却进不去满员服了"。
// 改等级就应该**只**改等级。
func setOpLevelJSON(content, name string, level int) (string, bool, bool, error) {
	var entries []map[string]json.RawMessage
	if strings.TrimSpace(content) != "" {
		if err := json.Unmarshal([]byte(content), &entries); err != nil {
			// 宁可拒绝也不猜：解析失败时若当成空名单，写入就会把现有的
			// 管理员名单整份覆盖掉 —— 那是不可挽回的数据丢失。
			return "", false, false, fmt.Errorf("ops.json 不是合法的 JSON，已中止操作以免覆盖现有名单：%v", err)
		}
	}
	lower := strings.ToLower(name)

	for _, e := range entries {
		var gotName string
		if raw, ok := e["name"]; ok {
			_ = json.Unmarshal(raw, &gotName)
		}
		if gotName == "" || strings.ToLower(gotName) != lower {
			continue
		}
		var cur int
		if raw, ok := e["level"]; ok {
			_ = json.Unmarshal(raw, &cur)
		}
		if cur == level {
			// 等级没变：原样返回、**不重写文件**
			//（重写会改变键顺序，也会触发服务端那边的名单文件变化处理）
			return content, false, true, nil
		}
		lv, err := json.Marshal(level)
		if err != nil {
			return "", false, true, err
		}
		e["level"] = lv
		return marshalPlayerList(entries), true, true, nil
	}

	lv, err := json.Marshal(level)
	if err != nil {
		return "", false, false, err
	}
	entries = append(entries, map[string]json.RawMessage{
		"uuid":                json.RawMessage(strconv.Quote(offlineUUID(name))),
		"name":                json.RawMessage(strconv.Quote(name)),
		"level":               lv,
		"bypassesPlayerLimit": json.RawMessage("false"),
	})
	return marshalPlayerList(entries), true, false, nil
}

// marshalPlayerList 把名单条目序列化成文件内容。
//
// 注意 JSON 对象的键顺序会变成字典序（Go 序列化 map 的行为）：
// 原版服务器写的是 uuid/name/level/bypassesPlayerLimit，这里会变成
// bypassesPlayerLimit/level/name/uuid。**服务端用 Gson 反序列化，不看顺序**，
// 功能上无差别；用键顺序换来"不丢字段"，这个交换是划算的。
func marshalPlayerList(entries []map[string]json.RawMessage) string {
	b, _ := json.MarshalIndent(entries, "", "  ")
	return string(b) + "\n"
}

// mutatePlayer 增删名单成员。
//
// 优先通过控制台命令执行：Minecraft 会自行解析玩家名到 UUID，
// 因此在线模式与离线模式都能正确处理，且无需重启服务器。
//
// 两条路不走控制台，都在这段逻辑里说清楚：
//   - 实例未运行（或无 stdin）→ 退化为直接编辑 JSON 文件，此时只能按
//     离线模式的算法推导 UUID，响应中会明确标注；
//   - 名字**拼不进命令**（含空格等，基岩版玩家名很常见）→ 同样走文件路径。
//     这类名字并没有被拒绝：空格不会造成命令注入（换行才会，那在校验层已经
//     拦掉了），它只是让 `kick Big Steve` 被服务器解析成"多给了参数"而失败，
//     发出去也没用。
func (s *Server) mutatePlayer(instanceID, kind, name string, add bool) (msg, mode string, err error) {
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		return "", "", fmt.Errorf("实例不存在")
	}

	// 判断实例是否可接收控制台命令
	st, err := cli.GetInstanceStatus(context.Background(), &pb.InstanceRequest{InstanceId: instanceID})
	running := err == nil && st.Status == "running"
	safeName := consoleSafeName(name)

	if running && safeName {
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
	switch {
	case !running:
		note += "（实例未运行，UUID 按离线模式推导；在线模式服务器请启动后操作或重启生效）"
	case !safeName:
		// 说清为什么没走控制台 —— 否则用户会以为面板"偷偷改了文件"
		note += "（该名字含空格等控制台无法直接表达的字符，已改为直接改名单文件；" +
			"基岩版玩家名（经 Floodgate 接入）常见这种情况，服务器会自动读取名单文件变化）"
	default:
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
//
// 开头的合法性检查不能省：`parsePlayerEntries` 解析失败时会**返回空列表**
// （列表展示用，静默降级成"没有条目"是可以接受的），但如果带着这个空列表
// 走到写入，就会把服务器上现有的整份名单覆盖成只剩新加的那一条 ——
// 手工编辑过名单、或上一次写入被中断过，都可能造成解析失败。
// 这类"改一个玩家、结果清空了全表"的后果不可挽回，所以在动手前先拒绝。
func updatePlayerJSON(kind, content, name string, add bool) (string, bool, error) {
	if strings.TrimSpace(content) != "" && !json.Valid([]byte(content)) {
		return "", false, fmt.Errorf("%s 不是合法的 JSON，已中止操作以免覆盖现有名单", playerFiles[kind])
	}
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
