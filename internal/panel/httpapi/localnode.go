package httpapi

import (
	"net"
	"net/http"
	"os"
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
