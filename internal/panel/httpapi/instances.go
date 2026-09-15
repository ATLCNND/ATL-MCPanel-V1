package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sync"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ---- 实例 CRUD ----

type createInstanceReq struct {
	NodeID       int64  `json:"node_id"`
	InstanceID   string `json:"instance_id"`
	Name         string `json:"name"`
	McType       string `json:"mc_type"`
	CoreType     string `json:"core_type"`
	JavaVersion  string `json:"java_version"`
	Port         int32  `json:"port"`
	MaxMem       string `json:"max_mem"`
	MinMem       string `json:"min_mem"`
	JarURL       string `json:"jar_url"`       // jar 路径（Daemon 本地绝对路径）
	StartCommand string `json:"start_command"` // 自定义启动命令模板
	CPUQuota     int32  `json:"cpu_quota"`     // CPU 配额百分比（100 = 1 核；0 = 不限制）
	BackupDir    string `json:"backup_dir"`    // 备份存放目录（空则用节点配置的 backup_root）
	MemLimit     string `json:"mem_limit"`     // cgroup 内存上限（如 "4G"；空 = 不限制）
	DiskLimitMB  int64  `json:"disk_limit_mb"` // 实例目录软配额（MB，0 = 不限制）
	DiskAutostop bool   `json:"disk_autostop"` // 超限时是否自动停止实例
	// 创建时按线路申请的穿透端口数：[{frps_id, count}]
	//
	// 放在创建时而不是之后补：多端口的需求来自模组/插件（BlueMap、Geyser 等），
	// 用户开服时就知道自己要几个；事后一个个加反而容易漏。
	Tunnels []struct {
		FrpsID int64 `json:"frps_id"`
		Count  int   `json:"count"`
	} `json:"tunnels"`

	// ---- 以下仅内部使用，不从请求体读取 ----
	//
	// 创建者身份由服务端从令牌上下文填充，用于端口配额校验。
	// 标记 json:"-" 是必须的：若可从请求体传入，客户端就能自称 admin 绕过配额。
	createdBy     int64  `json:"-"`
	createdByRole string `json:"-"`
}

// instanceIDRe 实例 ID 的合法形态。
//
// 限制成这个字符集不是洁癖：实例 ID 会直接成为**节点上的目录名**
//（`<instance_dir>/<ID>/`），放行 `../` 或 `/` 就能跳出实例根目录写文件。
// 同时排除大写也能避免"Linux 上建得出来、将来换到大小写不敏感的文件系统
// 就撞车"这类问题。
var instanceIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req createInstanceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	// 身份从令牌上下文取，**不信任请求体**（否则可以自称 admin 绕过端口配额）
	req.createdBy = currentUserID(r)
	req.createdByRole = roleOf(r)
	if req.NodeID == 0 || req.InstanceID == "" {
		writeErr(w, http.StatusBadRequest, "node_id 和 instance_id 必填")
		return
	}
	// 谁能往这个节点上建实例：总管理员（任何节点）或该节点的节点用户。
	// 普通用户一律 403 —— 这是节点用户与普通用户的唯一实质差别。
	if !s.canManageNode(currentUserID(r), roleOf(r), req.NodeID) {
		if isNodeUserRole(roleOf(r)) {
			writeErr(w, http.StatusForbidden, "你未被授权在该节点上创建实例（请联系总管理员分配节点）")
		} else {
			writeErr(w, http.StatusForbidden, "仅管理员或该节点的节点用户可创建实例")
		}
		return
	}
	// 实例 ID 校验（前端也会挡一道，但后端才是权威）
	if !instanceIDRe.MatchString(req.InstanceID) {
		writeErr(w, http.StatusBadRequest,
			"实例 ID 只能用小写字母、数字、连字符与下划线，且必须以字母或数字开头，最长 32 个字符")
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		writeErr(w, http.StatusBadRequest, "端口需为 1 ~ 65535 之间的整数")
		return
	}
	// 实例 ID 全局唯一（跨节点）：节点只保证自己目录下不重名，
	// 但面板侧用实例 ID 作为主键与授权、隧道的关联键，重名会让关联串味。
	var dup int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE instance_id = ?`, req.InstanceID).Scan(&dup); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if dup > 0 {
		writeErr(w, http.StatusConflict, "实例 ID「"+req.InstanceID+"」已被占用，请换一个")
		return
	}
	// 同节点端口唯一：两个实例绑同一个端口时，后启动的那个会直接失败，
	// 而报错来自服务端日志深处，排查成本高 —— 在这里挡掉更省事。
	var portUsed string
	if err := s.db.QueryRow(
		`SELECT instance_id FROM instances WHERE node_id = ? AND port = ? LIMIT 1`,
		req.NodeID, req.Port).Scan(&portUsed); err == nil && portUsed != "" {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("该节点上端口 %d 已被实例 %s 占用", req.Port, portUsed))
		return
	}
	// core_type 未填时回退到 mc_type
	coreType := req.CoreType
	if coreType == "" {
		coreType = req.McType
	}
	if coreType == "" {
		coreType = "custom"
	}

	// 端口配额**必须在建实例之前**校验。
	//
	// 曾经把它放在建完之后（想着"失败了也别把实例回滚"），结果申请 3 个端口
	// 而配额只有 2 个时：实例照样被创建，端口一个也没开 —— 用户得到一个
	// 不知所谓的空实例，还占掉了端口/实例 ID，重试又撞端口冲突。
	// "要的比配额多"是用户当场就能改的输入错误，就该在建任何东西之前挡回去；
	// 而"端口段被占满"是环境问题，那种才适合建完再告警（见 provisionInstanceTunnels）。
	if err := s.checkPortQuota(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 调 Daemon 创建实例
	cli, err := s.nodes.GetClient(req.NodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := cli.CreateInstance(context.Background(), &pb.CreateInstanceRequest{
		InstanceId:   req.InstanceID,
		Name:         req.Name,
		McType:       coreType,
		CoreType:     coreType,
		JavaVersion:  req.JavaVersion,
		Port:         req.Port,
		MaxMem:       req.MaxMem,
		MinMem:       req.MinMem,
		JarUrl:       req.JarURL,
		StartCommand: req.StartCommand,
		CpuQuota:     req.CPUQuota,
		BackupDir:    req.BackupDir,
		MemLimit:     req.MemLimit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}

	// 写 instances 表
	if _, err := s.db.Exec(
		`INSERT INTO instances (instance_id, node_id, name, mc_type, core_type, java_version, port, max_mem, start_command, cpu_quota, mem_limit, disk_limit_mb, disk_autostop, created_by, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'stopped')`,
		req.InstanceID, req.NodeID, req.Name, coreType, coreType, req.JavaVersion, req.Port, req.MaxMem, req.StartCommand, req.CPUQuota, req.MemLimit,
		req.DiskLimitMB, boolInt(req.DiskAutostop), currentUserID(r),
	); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 创建者若为普通用户则自动成为 owner；管理员默认拥有全部权限，无需归属记录
	if role := roleOf(r); role != RoleAdmin {
		_, _ = s.db.Exec(
			`INSERT INTO instance_assignments (instance_id, user_id, level) VALUES (?, ?, 'owner')
			 ON CONFLICT(instance_id, user_id) DO UPDATE SET level = 'owner'`,
			req.InstanceID, currentUserID(r))
	}

	// 按线路申请穿透端口。
	//
	// 失败**不回滚整个创建**：实例已经建好了，端口没开成属于"部分成功"，
	// 应当把原因带回给用户（配额不足？端口段耗尽？），让他在实例页补，
	// 而不是把实例一起删掉让他重来一遍。
	tunnelMsg, tunnelErr := s.provisionInstanceTunnels(req)

	detail := "名称=" + req.Name + " 核心=" + coreType
	if tunnelMsg != "" {
		detail += " " + tunnelMsg
	}
	s.audit(r, "create_instance", req.InstanceID, detail)

	created := map[string]interface{}{"instance_id": req.InstanceID, "message": "创建成功"}
	if tunnelMsg != "" {
		created["tunnel_message"] = tunnelMsg
	}
	if tunnelErr != nil {
		// 用 201 而不是错误码：实例确实创建成功了，只是端口没全开。
		// 前端会把 tunnel_error 显示成警告，而不是当成"创建失败"。
		created["message"] = "创建成功，但穿透端口未全部开通：" + tunnelErr.Error()
		created["tunnel_error"] = tunnelErr.Error()
	}
	writeJSON(w, http.StatusCreated, created)
}

// checkPortQuota 校验本次申请的端口数是否在配额内。
//
// 单独提出来是为了能在**创建实例之前**调用（见 handleCreateInstance 的注释）。
func (s *Server) checkPortQuota(req createInstanceReq) error {
	if len(req.Tunnels) == 0 || req.createdByRole == RoleAdmin {
		return nil
	}
	want := map[int64]int{}
	for _, t := range req.Tunnels {
		if t.FrpsID == 0 || t.Count <= 0 {
			continue
		}
		want[t.FrpsID] += t.Count
	}
	if len(want) == 0 {
		return nil
	}
	usage := s.portUsage(req.createdBy)
	for frpsID, n := range want {
		var quota int
		var lineName string
		if err := s.db.QueryRow(
			`SELECT np.quota, COALESCE(f.name, '') FROM node_user_ports np
			 LEFT JOIN frps_servers f ON f.id = np.frps_id
			 WHERE np.user_id = ? AND np.frps_id = ?`,
			req.createdBy, frpsID).Scan(&quota, &lineName); err != nil {
			return fmt.Errorf("你没有被分配线路 %d 的端口配额，请联系总管理员", frpsID)
		}
		if usage[frpsID]+n > quota {
			return fmt.Errorf("线路「%s」的端口不够：配额 %d 个，已用 %d 个，本次申请 %d 个（可先关闭不再需要的端口，或申请更多配额）",
				lineName, quota, usage[frpsID], n)
		}
	}
	return nil
}

// provisionInstanceTunnels 按创建请求里的 tunnels 申请公网端口并建立隧道。
//
// 前置条件：调用方已通过 checkPortQuota。这里只处理**分配阶段**的失败
//（端口段耗尽、下发失败），那种情况不回滚实例 —— 实例本身是好的，
// 端口之后可以在实例页补开。
//
// 返回 (人类可读的摘要, 首个错误)。
func (s *Server) provisionInstanceTunnels(req createInstanceReq) (string, error) {
	if len(req.Tunnels) == 0 {
		return "", nil
	}

	// 把请求归一化：同一线路可能被写了两遍，合并计数
	want := map[int64]int{}
	var order []int64
	for _, t := range req.Tunnels {
		if t.FrpsID == 0 || t.Count <= 0 {
			continue
		}
		if _, ok := want[t.FrpsID]; !ok {
			order = append(order, t.FrpsID)
		}
		want[t.FrpsID] += t.Count
	}
	if len(order) == 0 {
		return "", nil
	}

	// created：端口已分配并落库（含"已登记、待实例启动生效"）
	// failed ：真的失败了（下发报错）
	// pending：已登记但实例没在跑，frpc 要等实例启动才起
	created, failed, pending := 0, 0, 0
	var firstErr error
	for _, frpsID := range order {
		var frpsHost, frpsToken string
		var bindPort, portStart, portEnd int32
		if err := s.db.QueryRow(`SELECT host, bind_port, token, port_start, port_end FROM frps_servers WHERE id = ?`, frpsID).
			Scan(&frpsHost, &bindPort, &frpsToken, &portStart, &portEnd); err != nil {
			failed += want[frpsID]
			if firstErr == nil {
				firstErr = fmt.Errorf("线路 %d 不存在", frpsID)
			}
			continue
		}
		for i := 0; i < want[frpsID]; i++ {
			remote, err := s.allocRemotePort(frpsID, portStart, portEnd)
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			tunnelID := fmt.Sprintf("%s-tcp-%d", req.InstanceID, remote)
			name := fmt.Sprintf("%s-%d", req.Name, remote)
			// local_port 默认指向实例的游戏端口：这样隧道立刻是**可用**的
			//（不会出现"开了个口子但什么都没转发"的中间状态）。
			// 多端口模组（BlueMap 8123 / Geyser 19132 等）的真实本地端口，
			// 用户之后在实例页的「公网端口」里改即可。
			if _, err := s.db.Exec(`
				INSERT INTO tunnels (tunnel_id, instance_id, frps_id, name, protocol, local_port, remote_port, status)
				VALUES (?, ?, ?, ?, 'tcp', ?, ?, 'pending')`,
				tunnelID, req.InstanceID, frpsID, name, req.Port, remote); err != nil {
				failed++
				if firstErr == nil {
					firstErr = fmt.Errorf("写入隧道失败: %w", err)
				}
				continue
			}
			status, msg := s.daemonApplyTunnel(req.InstanceID, tunnelID, name, "tcp", req.Port, remote, "",
				frpsHost, bindPort, frpsToken)
			_, _ = s.db.Exec(`UPDATE tunnels SET status = ? WHERE tunnel_id = ?`, status, tunnelID)
			// 只看 "error" 才算失败。
			//
			// 实例刚建好时是**停止**的，而第 16 项之后"给停止的实例开端口"是
			// 只登记、不启动 frpc → 下发结果就是 stopped。端口其实已经分配并落库了，
			// 实例一启动就会生效，把这种正常状态报成"未成功"是误导
			//（曾经这里只认 running，于是"建实例时开端口"总会附带一句失败提示）。
			switch status {
			case "error":
				failed++
				if firstErr == nil {
					firstErr = fmt.Errorf("%s", msg)
				}
			case "running":
				created++
			default: // stopped / unknown：已登记，待实例启动生效
				created++
				pending++
			}
		}
	}

	summary := fmt.Sprintf("已开通 %d 个公网端口", created)
	if pending > 0 {
		summary += fmt.Sprintf("（其中 %d 个待实例启动后生效）", pending)
	}
	if failed > 0 {
		summary += fmt.Sprintf("，%d 个未成功", failed)
	}
	return summary, firstErr
}

// instanceView 实例列表项。
type instanceView struct {
	ID         int64  `json:"id"`
	InstanceID string `json:"instance_id"`
	NodeID     int64  `json:"node_id"`
	Name       string `json:"name"`
	CoreType   string `json:"core_type"`
	Port       int32  `json:"port"`
	MaxMem     string `json:"max_mem"`
	Status     string `json:"status"`      // 面板记录的期望状态
	LiveStatus string `json:"live_status"` // Daemon 上报的实时状态；unknown 表示节点不可达
	// IconMtime 实例图标的修改时间（Unix 秒，0 = 没有图标）。
	// 前端据此决定要不要请求 `/api/instances/{id}/icon`，并把它当缓存击穿参数
	// —— 没有它的话列表页每行都会去请求一次注定 404 的图标。
	IconMtime  int64  `json:"icon_mtime"`
	Level      string `json:"level"`       // 当前用户对该实例的权限级别
	CPUQuota   int    `json:"cpu_quota"`   // CPU 配额百分比（100 = 1 核；0 = 不限制）
	MemLimit   string `json:"mem_limit"`   // cgroup 内存上限（空 = 不限制）
	DiskLimitMB  int64  `json:"disk_limit_mb"` // 磁盘软配额（MB，0 = 不限制）
	DiskAutostop bool   `json:"disk_autostop"` // 超限自动停机
	// 创建时按线路申请的穿透端口数：[{frps_id, count}]
	//
	// 放在创建时而不是之后补：多端口的需求来自模组/插件（BlueMap、Geyser 等），
	// 用户开服时就知道自己要几个；事后一个个加反而容易漏。
	Tunnels []struct {
		FrpsID int64 `json:"frps_id"`
		Count  int   `json:"count"`
	} `json:"tunnels"`
	// 到期控制
	ExpiresAt     string `json:"expires_at"`          // RFC3339；空 = 永不到期
	ExpiryDays    int    `json:"expiry_notice_days"`  // 提前多少天告警
	ExpiryAutostop bool  `json:"expiry_autostop"`     // 到期是否自动停止
	ExpiryState   string `json:"expiry_state"`        // none / active / soon / expired
	ExpiryDaysLeft int   `json:"expiry_days_left"`    // 剩余天数（已到期为负）
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	role, _ := r.Context().Value(ctxKeyRole).(string)

	// 可见范围：
	//   admin      → 全部
	//   其它角色   → 被显式授权的实例
	//   节点用户 → 上述**并集**"自己在管节点上的全部实例"
	//                （否则在他管的节点上、但归属别人的实例会看不到，
	//                 而他恰恰要对那台机器负责）
	const cols = `i.id, i.instance_id, i.node_id, i.name, i.core_type, i.port, i.max_mem, i.status,
	              i.cpu_quota, i.mem_limit, i.disk_limit_mb, i.disk_autostop,
	              i.expires_at, i.expiry_notice_days, i.expiry_autostop`

	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case role == RoleAdmin:
		rows, err = s.db.Query(`SELECT ` + cols + ` FROM instances i ORDER BY i.id`)
	case isNodeUserRole(role):
		rows, err = s.db.Query(`
			SELECT `+cols+`
			FROM instances i
			WHERE i.instance_id IN (SELECT instance_id FROM instance_assignments WHERE user_id = ?)
			   OR i.node_id IN (SELECT node_id FROM node_users WHERE user_id = ?)
			ORDER BY i.id`, userID, userID)
	default:
		rows, err = s.db.Query(`
			SELECT `+cols+`
			FROM instances i
			JOIN instance_assignments a ON a.instance_id = i.instance_id
			WHERE a.user_id = ?
			ORDER BY i.id`, userID)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []*instanceView{}
	for rows.Next() {
		v := &instanceView{}
		var diskAutostop int
		var expires sql.NullTime
		if err := rows.Scan(&v.ID, &v.InstanceID, &v.NodeID, &v.Name, &v.CoreType, &v.Port, &v.MaxMem,
			&v.Status, &v.CPUQuota, &v.MemLimit, &v.DiskLimitMB, &diskAutostop,
			&expires, &v.ExpiryDays, &v.ExpiryAutostop); err != nil {
			continue
		}
		applyInstanceExtras(s, userID, role, v, diskAutostop, expires)
		list = append(list, v)
	}
	rows.Close()

	s.reconcileInstanceStatus(r.Context(), list)
	writeJSON(w, http.StatusOK, list)
}

// applyInstanceExtras 补齐扫描后的派生字段（避免两处分支各写一遍）。
func applyInstanceExtras(s *Server, userID int64, role string, v *instanceView, diskAutostop int, expires sql.NullTime) {
	v.DiskAutostop = diskAutostop == 1
	v.Level, _ = s.instanceLevel(userID, role, v.InstanceID)
	// 节点用户对自己管的实例至少是 owner（否则连控制台都进不去，
	// 而"能创建"却"不能用"显然说不通）
	if isNodeUserRole(role) && v.Level == "" && s.canManageNode(userID, role, v.NodeID) {
		v.Level = LevelOwner
	}
	v.ExpiresAt, v.ExpiryState, v.ExpiryDaysLeft = expiryView(expires)
}

// expiryView 把到期时间换算成前端好用的三件套。
func expiryView(expires sql.NullTime) (at, state string, daysLeft int) {
	if !expires.Valid {
		return "", "none", 0
	}
	at = expires.Time.Format(time.RFC3339)
	left := time.Until(expires.Time)
	// 向上取整：还剩 47 小时时显示"还有 1 天"会让人以为明天就到期，
	// 而实际是后天 —— 这类"看着少一天"的偏差会直接导致管理员误判时间。
	daysLeft = int(math.Ceil(left.Hours() / 24))
	switch {
	case left <= 0:
		state = "expired"
	case left <= 72*time.Hour:
		state = "soon"
	default:
		state = "active"
	}
	return at, state, daysLeft
}

// reconcileInstanceStatus 用 Daemon 上报的实时状态校正实例状态。
//
// 必要性：数据库中的 status 只反映面板发出的启停动作。若 Minecraft 进程
// 崩溃或被系统 OOM 杀死，面板仍会显示「运行中」。以 Daemon 实际探测结果为准，
// 可自愈这类不一致（同时回写数据库，使其它查询也保持一致）。
func (s *Server) reconcileInstanceStatus(ctx context.Context, list []*instanceView) {
	const maxConcurrent = 8

	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup

	for _, v := range list {
		wg.Add(1)
		go func(v *instanceView) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 单实例查询限时，避免节点无响应时拖慢整个列表接口
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()

			cli, err := s.nodes.GetClient(v.NodeID)
			if err != nil {
				v.LiveStatus = "unknown"
				return
			}
			resp, err := cli.GetInstanceStatus(cctx, &pb.InstanceRequest{InstanceId: v.InstanceID})
			if err != nil || resp.Status == "" {
				v.LiveStatus = "unknown"
				return
			}
			v.LiveStatus = resp.Status
			v.IconMtime = resp.IconMtime

			// 自愈：数据库状态与实际不符时回写
			if resp.Status != v.Status && resp.Status != "not_found" {
				if _, err := s.db.Exec(`UPDATE instances SET status = ? WHERE instance_id = ?`,
					resp.Status, v.InstanceID); err == nil {
					v.Status = resp.Status
				}
			}
		}(v)
	}
	wg.Wait()
}

// instanceAction 通用操作：start/stop/restart/delete。
func (s *Server) handleInstanceAction(w http.ResponseWriter, r *http.Request, action string) {
	instanceID := r.PathValue("id") // 这里是 instance_id 字符串

	// 权限：删除需要「总管理员」或「该节点的节点用户」；
	// 启停/重启需要 collab 及以上（不变）
	if action == "delete" {
		if !s.canManageInstance(currentUserID(r), roleOf(r), instanceID) {
			writeErr(w, http.StatusForbidden, "仅总管理员或该节点的节点用户可删除实例")
			return
		}
		if !s.instanceExists(instanceID) {
			writeErr(w, http.StatusNotFound, "实例不存在")
			return
		}
	} else if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}

	// 从 DB 拿 node_id
	nodeID, ok := s.instanceNodeID(instanceID)
	if !ok {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	req := &pb.InstanceRequest{InstanceId: instanceID}
	// remove_files：把实例目录（世界存档、插件、jar、备份）一并删除。
	// 默认 false —— 只注销实例、保留文件，避免误点造成不可恢复的损失。
	removeFiles := r.URL.Query().Get("remove_files") == "1" || r.URL.Query().Get("remove_files") == "true"
	var resp *pb.OperationResponse
	switch action {
	case "start":
		resp, err = cli.StartInstance(context.Background(), req)
	case "stop":
		resp, err = cli.StopInstance(context.Background(), req)
	case "kill":
		resp, err = cli.KillInstance(context.Background(), req)
	case "restart":
		resp, err = cli.RestartInstance(context.Background(), req)
	case "delete":
		resp, err = cli.DeleteInstance(context.Background(), &pb.DeleteInstanceRequest{
			InstanceId: instanceID, RemoveFiles: removeFiles,
		})
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}

	// 更新状态：优先向 Daemon 确认实际状态，避免仅凭动作推断导致不一致
	newStatus := "stopped"
	if action == "start" || action == "restart" {
		newStatus = "running"
	}
	if action == "delete" {
		_, _ = s.db.Exec(`DELETE FROM instances WHERE instance_id = ?`, instanceID)
		_, _ = s.db.Exec(`DELETE FROM instance_assignments WHERE instance_id = ?`, instanceID)
		// 一并清理从属记录：残留会让调度器继续为已删除的实例备份并产生告警，
		// 也会阻止其引用的备份策略被删除。
		_, _ = s.db.Exec(`DELETE FROM backup_schedules WHERE instance_id = ?`, instanceID)
		_, _ = s.db.Exec(`DELETE FROM tunnels WHERE instance_id = ?`, instanceID)
	} else {
		// restart 是异步完成的（Daemon 需等待进程退出），此时查询可能仍是旧状态，
		// 因此仅对 start/stop 做即时确认
		if action != "restart" {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if st, err := cli.GetInstanceStatus(cctx, req); err == nil && st.Status != "" && st.Status != "not_found" {
				newStatus = st.Status
			}
			cancel()
		}
		_, _ = s.db.Exec(`UPDATE instances SET status = ? WHERE instance_id = ?`, newStatus, instanceID)
	}

	s.audit(r, action+"_instance", instanceID, resp.Message)
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message, "status": newStatus})
}