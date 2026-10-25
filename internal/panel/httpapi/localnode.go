package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 本机节点的探测与「一键部署本机节点」（底账 F19 / HANDOFF-2026-10-02 §③）。
//
// 背景：面板和节点常常在同一台机器上（内测就是这么跑的）。这种时候让用户去填
// "127.0.0.1 + root 的 SSH 密码"是荒唐的 —— 面板本来就有这台机器的权限。
// 所以这里做三件事：
//   ① 探测本机是否已有 daemon 在跑（看 gRPC 端口通不通、以及 systemd 单元状态）；
//   ② 若没有，允许"一键部署本机节点"（不走 SSH，直接在本机写配置、装单元、起服务）；
//   ③ 部署完自动登记成节点（地址用 127.0.0.1，凭据留空 —— 本机不需要）。
//
// ⚠️ 安全前提：这些接口都要求**总管理员**，并且只能操作本机（地址写死 127.0.0.1）。

// localNodeState 本机节点现状。
type localNodeState struct {
	// DaemonPort 探测到（或配置的）Daemon gRPC 端口
	DaemonPort int `json:"daemon_port"`
	// Listening 该端口上有人监听（大概率就是 daemon）
	Listening bool `json:"listening"`
	// Registered 面板的 nodes 表里是否已有指向本机的节点
	Registered bool `json:"registered"`
	NodeName   string `json:"node_name,omitempty"`
	// Hostname 本机名（登记时作为默认节点名）
	Hostname string `json:"hostname"`
	// CanAutoDeploy 是否具备"直接在本机安装"的条件（能写安装目录 + 有 systemd）
	CanAutoDeploy bool   `json:"can_auto_deploy"`
	Message       string `json:"message,omitempty"`
}

// handleLocalNodeState GET /api/nodes/local
//
// 给"节点管理"页用：如果本机还没接入，界面直接给一个「一键部署本机节点」按钮，
// 而不是让用户去填 SSH 密码。
func (s *Server) handleLocalNodeState(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	port := s.daemonPortFromListen()
	st := localNodeState{DaemonPort: port}
	if h, err := os.Hostname(); err == nil {
		st.Hostname = h
	}

	// 端口通不通 = 本机有没有 daemon 在跑
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 800*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		st.Listening = true
	}

	// 是否已经登记过（地址是回环就算同一台机器）
	rows, err := s.db.Query(`SELECT name, ip FROM nodes`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name, ip string
			if rows.Scan(&name, &ip) == nil && isLocalNodeHost(ip) {
				st.Registered = true
				st.NodeName = name
				break
			}
		}
	}

	// 能不能直接在本机装：安装目录可写 + 有 systemd（面板本身就跑在 systemd 下，
	// 这条基本恒真；但容器化部署的面板里不一定，所以还是探一下）
	if err := os.MkdirAll(s.remoteInstallDir(), 0o755); err == nil {
		if f, err := os.CreateTemp(s.remoteInstallDir(), ".probe-*"); err == nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			st.CanAutoDeploy = dirHasSystemd()
		}
	}
	if !st.CanAutoDeploy {
		st.Message = "本机不具备自动安装条件（需要可写的安装目录与 systemd），请手动安装节点"
	}
	writeJSON(w, http.StatusOK, st)
}

// dirHasSystemd 判断能不能用 systemctl（容器里通常不行）。
func dirHasSystemd() bool {
	for _, p := range []string{"/run/systemd/system", "/usr/bin/systemctl", "/bin/systemctl"} {
		if _, err := os.Stat(p); err == nil {
			// /run/systemd/system 存在基本就是 systemd 在跑
			if strings.HasPrefix(p, "/run/") {
				return true
			}
		}
	}
	// 退一步：能执行 systemctl 也算
	if _, err := os.Stat("/usr/bin/systemctl"); err == nil {
		return true
	}
	if _, err := os.Stat("/bin/systemctl"); err == nil {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 本机节点的「接入」与「一键部署」（底账 F19/F20 的落地，preview M1）
// ---------------------------------------------------------------------------

// localInstallState 描述本机安装的前置条件与结果。
type localStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// handleAdoptLocalNode POST /api/nodes/local/adopt
//
// 把**本机正在运行的 Daemon** 直接登记成节点（ip=127.0.0.1），不需要任何 SSH 凭据。
// 面板本来就有这台机器的权限，让用户去填 127.0.0.1 + root 密码是多余的。
func (s *Server) handleAdoptLocalNode(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	port := s.daemonPortFromListen()
	if !portListening("127.0.0.1", port) {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("本机 %d 端口没有 Daemon 在监听；请先用「一键部署本机节点」安装，或检查 daemon 服务状态", port))
		return
	}
	node, created, err := s.adoptLocalNode(port)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if created {
		s.audit(r, "adopt_local_node", node.Name, "ip=127.0.0.1")
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"created":    created,
		"node":       node,
		"duplicates": s.otherLocalNodes(node.ID),
		"message": map[bool]string{true: "已把本机 Daemon 登记为节点", false: "本机节点已存在，已刷新端口配置"}[created],
	})
}

// adoptLocalNode 确保 nodes 表里有一条指向本机的记录；返回该记录与是否新建。
func (s *Server) adoptLocalNode(port int) (nodeView, bool, error) {
	var v nodeView
	var exists bool
	rows, err := s.db.Query(`SELECT id, name, ip, COALESCE(grpc_port,0) FROM nodes`)
	if err != nil {
		return v, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name, ip string
		var gp int
		if rows.Scan(&id, &name, &ip, &gp) == nil && isLocalNodeHost(ip) {
			v.ID, v.Name, v.IP, v.DaemonGRPCPort, exists = id, name, ip, gp, true
			break
		}
	}
	if exists {
		if v.DaemonGRPCPort != port {
			_, _ = s.db.Exec(`UPDATE nodes SET grpc_port = ? WHERE id = ?`, port, v.ID)
			v.DaemonGRPCPort = port
		}
		// 标上「本机探测接入」——界面上决定哪些操作可用（本机节点不需要、也不该提示配置凭据）。
		// 只在为空时写：用户可能手动标过别的来源，不要覆盖他的判断。
		_, _ = s.db.Exec(`UPDATE nodes SET enroll_mode = 'local-probe' WHERE id = ? AND COALESCE(enroll_mode,'') = ''`, v.ID)
		_ = s.db.QueryRow(`SELECT COALESCE(public_ip,''), ssh_user, ssh_port, COALESCE(ssh_auth_type,'')
			FROM nodes WHERE id = ?`, v.ID).Scan(&v.PublicIP, &v.SSHUser, &v.SSHPort, &v.AuthType)
		v.AuthType = "" // 本机节点本来就不需要凭据
		return v, false, nil
	}

	// 新建：名字优先用 **Daemon 配置里的 node_id**（与它心跳时报的保持一致，否则会出两行），
	// 其次才回退到主机名。重名就加后缀（name 只是显示名，不是身份）。
	base := localDaemonNodeID()
	if base == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			base = h
		}
	}
	if base == "" {
		base = "local"
	}
	name := base
	for i := 2; i < 50; i++ {
		var n int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name = ?`, name).Scan(&n)
		if n == 0 {
			break
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	res, err := s.db.Exec(`INSERT INTO nodes (name, ip, public_ip, ssh_user, ssh_auth, ssh_auth_type,
		ssh_port, grpc_port, status, enroll_mode)
		VALUES (?, '127.0.0.1', '', 'root', '', '', 22, ?, 'offline', 'local-probe')`,
		name, port)
	if err != nil {
		return v, false, err
	}
	id, _ := res.LastInsertId()
	return nodeView{ID: id, Name: name, IP: "127.0.0.1", SSHPort: 22, DaemonGRPCPort: port}, true, nil
}

// portListening 判断本机某端口是否有人监听。
func portListening(host string, port int) bool {
	if port <= 0 {
		return false
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 900*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// handleInstallLocalNode POST /api/nodes/local/install
//
// 「本机一键部署」：面板直接在本机装 Daemon（不走 SSH）。
//
// 为什么由面板自己做而不是调 deploy/install.sh：那份脚本假定自己处在**解压出来的包**
// 目录里，会从 $SRC_DIR/bin 往 $INSTALL_DIR/bin 复制；而在已安装布局里两者是同一个目录，
// 自我复制会失败。这里只做必要的事，并把每一步如实回报（哪步失败一眼能看出来）。
//
// 前置：以 root 运行（要写 /etc/systemd/system 与安装目录）。
func (s *Server) handleInstallLocalNode(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	if os.Geteuid() != 0 {
		writeErr(w, http.StatusForbidden, "本机一键部署需要面板以 root 运行（要写 /etc/systemd/system）")
		return
	}
	dir, err := localPanelDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法定位面板安装目录："+err.Error())
		return
	}

	steps := []localStep{}
	add := func(name string, ok bool, detail string) {
		steps = append(steps, localStep{Name: name, OK: ok, Detail: detail})
	}

	daemonPort := s.daemonPortFromListen()
	service := s.daemonServiceName
	if service == "" {
		service = "atlmcpanel-daemon"
	}

	// 1) 二进制：优先用面板配置里指定的 daemon_binary，其次安装目录下已有的
	binPath := filepath.Join(dir, "bin", "atl-daemon")
	if src, err := s.daemonBinaryPath(); err == nil && src != "" && src != binPath {
		if data, err := os.ReadFile(src); err == nil {
			if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err == nil {
				if err := os.WriteFile(binPath, data, 0o755); err != nil {
					add("安装 daemon 二进制", false, err.Error())
				} else {
					add("安装 daemon 二进制", true, binPath)
				}
			}
		}
	}
	if _, err := os.Stat(binPath); err != nil {
		add("安装 daemon 二进制", false, "未找到可用二进制（配置里没有 daemon_binary，安装目录下也没有 bin/atl-daemon）")
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"steps": steps, "error": "缺少 daemon 二进制"})
		return
	} else if len(steps) == 0 {
		add("安装 daemon 二进制", true, "已存在："+binPath)
	}

	// 2) 配置：缺 daemon 段就追加（**不重写整份 config.yaml**，注释与既有配置一律保留）
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBody, _ := os.ReadFile(cfgPath)
	if !hasTopLevelKey(string(cfgBody), "daemon") {
		host, _ := os.Hostname()
		if host == "" {
			host = "local"
		}
		block := fmt.Sprintf("\n# ---- Daemon（本机一键部署自动追加 %s）----\n"+
			"daemon:\n  node_id: %q\n  panel_address: %q\n  grpc_listen: %q\n  instance_dir: \"instances\"\n",
			time.Now().Format("2006-01-02 15:04"), host,
			"127.0.0.1:"+s.grpcPort(),
			"127.0.0.1:"+strconv.Itoa(daemonPort))
		if err := os.WriteFile(cfgPath, append(cfgBody, []byte(block)...), 0o600); err != nil {
			add("写入 daemon 配置段", false, err.Error())
		} else {
			add("写入 daemon 配置段", true, cfgPath)
		}
	} else {
		add("写入 daemon 配置段", true, "已存在，未改动")
	}

	// 3) systemd 单元：用包里那份模板，替换安装目录占位符
	unitSrc := filepath.Join(dir, "deploy", "systemd", service+".service")
	unitDst := "/etc/systemd/system/" + service + ".service"
	if raw, err := os.ReadFile(unitSrc); err != nil {
		add("安装 systemd 单元", false, "找不到单元模板 "+unitSrc)
	} else {
		unit := strings.ReplaceAll(string(raw), "__INSTALL_DIR__", dir)
		if err := os.WriteFile(unitDst, []byte(unit), 0o644); err != nil {
			add("安装 systemd 单元", false, err.Error())
		} else {
			add("安装 systemd 单元", true, unitDst)
		}
	}

	// 4) 启动并等它就绪
	if out, err := runCmd("systemctl", "daemon-reload"); err != nil {
		add("systemctl daemon-reload", false, out)
	} else {
		add("systemctl daemon-reload", true, "")
	}
	if out, err := runCmd("systemctl", "enable", "--now", service); err != nil {
		add("启动 "+service, false, out)
	} else {
		add("启动 "+service, true, "")
	}

	ready := false
	for i := 0; i < 20; i++ {
		if portListening("127.0.0.1", daemonPort) {
			ready = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	add("Daemon 端口就绪", ready, fmt.Sprintf("127.0.0.1:%d", daemonPort))

	var node nodeView
	created := false
	if ready {
		if n, c, err := s.adoptLocalNode(daemonPort); err == nil {
			node, created = n, c
			add("登记为节点", true, n.Name)
		} else {
			add("登记为节点", false, err.Error())
		}
	}

	s.audit(r, "install_local_node", service, joinSteps(steps))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ready":  ready,
		"steps":  steps,
		"node":   node,
		"created": created,
	})
}

// localDaemonNodeID 从本机 config.yaml 里读 daemon.node_id。
//
// 为什么需要它：Daemon 的心跳/注册用的是**它自己配置里的 node_id**（可能不等于主机名），
// 而面板按 node_id 建行。若登记时用主机名，同一台机器会出现两行（实测踩到）。
// 本机场景下这个文件就在面板眼前，直接读最准。
func localDaemonNodeID() string {
	dir, err := localPanelDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "node_id:") {
			v := strings.TrimSpace(strings.TrimPrefix(t, "node_id:"))
			v = strings.Trim(v, "\"'")
			if i := strings.IndexAny(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			return v
		}
	}
	return ""
}

// localPanelDir 面板所在安装目录（bin/atl-panel 的上一级）。
func localPanelDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	return filepath.Dir(filepath.Dir(exe)), nil
}

// hasTopLevelKey 粗判一份 YAML 里有没有某个顶层键（够用：只找行首非空白的 key:）。
func hasTopLevelKey(yamlText, key string) bool {
	for _, line := range strings.Split(yamlText, "\n") {
		if strings.HasPrefix(line, key+":") {
			return true
		}
	}
	return false
}

// runCmd 跑一条命令并返回合并输出（失败时用它在界面里展示原因）。
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// joinSteps 把步骤结果压成一行，供审计记录。
func joinSteps(steps []localStep) string {
	parts := make([]string, 0, len(steps))
	for _, st := range steps {
		mark := "ok"
		if !st.OK {
			mark = "FAIL"
		}
		parts = append(parts, st.Name+"="+mark)
	}
	return strings.Join(parts, " ")
}

// otherLocalNodes 列出**其它**指向本机的节点行（同一个 Daemon 被登记了两次时会出现）。
//
// 面板不自动删：那可能是用户真按 SSH 方式在同一台机器上登记的另一条记录。
// 只把名字报给界面，由人决定留哪条。
func (s *Server) otherLocalNodes(keepID int64) []string {
	rows, err := s.db.Query(`SELECT id, name, ip FROM nodes`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var name, ip string
		if rows.Scan(&id, &name, &ip) == nil && id != keepID && isLocalNodeHost(ip) {
			out = append(out, name)
		}
	}
	return out
}
