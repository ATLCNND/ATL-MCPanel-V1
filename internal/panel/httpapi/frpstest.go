package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 线路自检的 HTTP 入口（仅总管理员）。
//
//	POST /api/frps/test      body = 表单里那几个字段（**保存之前**就能测）
//	POST /api/frps/{id}/test 用已保存的线路配置测
//
// 自检**在节点上跑**（Daemon 的 TestFrps）：真正跑 frpc 的是节点，
// 面板能连通 ≠ 节点能连通 —— 只从面板测会得出"线路没问题"的错误结论。
func (s *Server) handleTestFrps(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	req, ok := s.readFrpsTestInput(w, r)
	if !ok {
		return
	}
	s.runFrpsTest(w, r, req)
}

// handleTestFrpsSaved POST /api/frps/{id}/test —— 用库里已保存的配置测。
func (s *Server) handleTestFrpsSaved(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "线路 id 无效")
		return
	}
	var host, token, authMode, frpsUser, lineType string
	var bindPort, portStart, portEnd int32
	if err := s.db.QueryRow(`SELECT host, bind_port, token, port_start, port_end,
			COALESCE(auth_mode,'auth'), COALESCE(user,''), COALESCE(line_type,'self_hosted')
		FROM frps_servers WHERE id = ?`, id).
		Scan(&host, &bindPort, &token, &portStart, &portEnd, &authMode, &frpsUser, &lineType); err != nil {
		writeErr(w, http.StatusNotFound, "线路不存在")
		return
	}

	// 公网直出（底账 F16）：这类线路**没有 frpc**，拿 frpc 去登录/注册毫无意义
	//（以前走同一套自检，于是永远失败）。改为按**节点实际能力**判定两件事：
	//   ① 线路写的地址是否就是这个节点自己的公网 IP（写错了玩家永远连不上）；
	//   ② 从节点实拨 线路地址:端口 通不通（端口有没有真的对外）。
	if frp.NormalizeLineType(lineType) == frp.LineDirect {
		s.runDirectLineTest(w, r, id, host)
		return
	}

	// 第三方单隧道（底账 F16）：自检**必须用服务商分配的代理名与远端端口**。
	//
	// 为什么：这两样都是服务端侧登记的，我们自己造一个上去会被拒
	// （实测自造 name/port → 服务端回「FRPC 配置文件错误」，看起来像网络/配置问题）。
	// 所以从该线路下已导入的隧道里取一组真实参数；没有隧道就只能测可达性。
	var proxyName, bwLimit, bwMode, tlsEnable, tlsFirstByte string
	var remotePort int32
	if frp.NormalizeLineType(lineType) == frp.LineThirdParty {
		_ = s.db.QueryRow(`SELECT COALESCE(name,''), COALESCE(remote_port,0),
				COALESCE(bandwidth_limit,''), COALESCE(bandwidth_limit_mode,''),
				COALESCE(tls_enable,''), COALESCE(tls_disable_custom_first_byte,'')
			FROM tunnels WHERE frps_id = ? AND remote_port > 0 ORDER BY id LIMIT 1`, id).
			Scan(&proxyName, &remotePort, &bwLimit, &bwMode, &tlsEnable, &tlsFirstByte)
	}

	s.runFrpsTest(w, r, frpsTestInput{
		Host: host, BindPort: bindPort, Token: token,
		PortStart: portStart, PortEnd: portEnd,
		AuthMode: authMode, User: frpsUser,
		ProxyName: proxyName, RemotePort: remotePort,
		// 传输参数也要带上：服务端把它当成隧道定义的一部分校验
		BandwidthLimit: bwLimit, BandwidthLimitMode: bwMode,
		// 传输层 TLS 同理（缺了会连上但无响应）
		TLSEnable: tlsEnable, TLSDisableCustomFirstByte: tlsFirstByte,
	})
}

// frpsTestInput 自检需要的字段。
type frpsTestInput struct {
	Host      string `json:"host"`
	BindPort  int32  `json:"bind_port"`
	Token     string `json:"token"`
	PortStart int32  `json:"port_start"`
	PortEnd   int32  `json:"port_end"`
	// NodeID 指定在哪个节点上测；0 = 自动挑第一个可用节点
	NodeID int64 `json:"node_id"`

	// ---- 第三方单隧道（底账 F16）----
	// AuthMode / User：登录方式与租户标识（导入时从服务商配置里学到的）
	AuthMode string `json:"auth_mode"`
	User     string `json:"user"`
	// RemotePort / ProxyName：服务商分配的远端端口与代理名。
	// RemotePort > 0 时自检用这组真实参数试注册，不再自己分配端口。
	RemotePort int32  `json:"remote_port"`
	ProxyName  string `json:"proxy_name"`
	// 服务商的传输参数：自检必须与生产下发完全一致，否则服务端照样拒
	BandwidthLimit     string `json:"bandwidth_limit"`
	BandwidthLimitMode string `json:"bandwidth_limit_mode"`
	// 传输层 TLS（三态，空 = 不写）
	TLSEnable                 string `json:"tls_enable"`
	TLSDisableCustomFirstByte string `json:"tls_disable_custom_first_byte"`
}

// readFrpsTestInput 解析并校验自检输入。
func (s *Server) readFrpsTestInput(w http.ResponseWriter, r *http.Request) (frpsTestInput, bool) {
	var in frpsTestInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return in, false
	}
	in.Host = strings.TrimSpace(in.Host)
	if in.Host == "" {
		writeErr(w, http.StatusBadRequest, "线路地址不能为空")
		return in, false
	}
	if in.BindPort <= 0 {
		in.BindPort = 7000
	}
	if in.PortStart <= 0 {
		in.PortStart = 25565
	}
	if in.PortEnd < in.PortStart {
		in.PortEnd = in.PortStart + 100
	}
	return in, true
}

// runFrpsTest 选节点 → 调 Daemon 自检 → 把结论整理成 JSON。
func (s *Server) runFrpsTest(w http.ResponseWriter, r *http.Request, in frpsTestInput) {
	nodeID := in.NodeID
	if nodeID == 0 {
		// 自动挑第一个节点：线路是全局的，任何一个装了 frpc 的节点都能验证它
		// （返回体里会写明是在哪个节点上测的，避免"换台机器结果不同"时无从对照）
		if err := s.db.QueryRow(`SELECT id FROM nodes ORDER BY id LIMIT 1`).Scan(&nodeID); err != nil {
			writeErr(w, http.StatusBadRequest, "还没有登记任何节点，无法自检（穿透是节点上的 frpc 在跑）")
			return
		}
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "节点连接失败："+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	resp, err := cli.TestFrps(ctx, &pb.TestFrpsRequest{
		Host:      in.Host,
		BindPort:  in.BindPort,
		Token:     in.Token,
		PortStart: in.PortStart,
		PortEnd:   in.PortEnd,
		// 第三方线路的真实参数（空/0 时 daemon 退回"自己挑端口"的老行为）
		RemotePort:         in.RemotePort,
		ProxyName:          in.ProxyName,
		User:               in.User,
		AuthMode:           in.AuthMode,
		BandwidthLimit:     in.BandwidthLimit,
		BandwidthLimitMode: in.BandwidthLimitMode,
		TlsEnable:                 in.TLSEnable,
		TlsDisableCustomFirstByte: in.TLSDisableCustomFirstByte,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "自检请求失败："+err.Error())
		return
	}

	var nodeName string
	_ = s.db.QueryRow(`SELECT name FROM nodes WHERE id = ?`, nodeID).Scan(&nodeName)

	s.audit(r, "test_frps", in.Host,
		"节点="+nodeName+" 结论="+conclusionOf(resp))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        resp.Success,
		"error":          resp.Error,
		"dial_ok":        resp.DialOk,
		"dial_ms":        resp.DialMs,
		"dial_error":     resp.DialError,
		"register_ok":    resp.RegisterOk,
		"used_port":      resp.UsedPort,
		"register_error": resp.RegisterError,
		"log_tail":       resp.LogTail,
		"node_id":        nodeID,
		"node_name":      nodeName,
		"address":        in.Host + ":" + strconv.Itoa(int(in.BindPort)),
	})
}

// conclusionOf 给审计日志用的一句话结论。
func conclusionOf(r *pb.TestFrpsResponse) string {
	switch {
	case r.Success:
		return "通过"
	case !r.DialOk:
		return "TCP 不可达"
	default:
		return "注册失败"
	}
}

// runDirectLineTest 公网直出线路的自检：不碰 frpc，只看节点与线路实况。
//
// 判据两条：
//   ① **地址一致性**：线路里填的主机应当就是这个节点的公网 IP ——
//      填错是最常见的错误，症状是玩家连不上，很容易被误判成防火墙问题。
//   ② **端口可达性**：从节点侧实拨 线路地址:<该线路下某条隧道的远端端口>。
//      刻意用节点自己拨（而不是面板），因为面板通常不在同一网络。
func (s *Server) runDirectLineTest(w http.ResponseWriter, r *http.Request, lineID int64, host string) {
	var remotePort int32
	_ = s.db.QueryRow(`SELECT remote_port FROM tunnels WHERE frps_id = ? AND remote_port > 0 ORDER BY id LIMIT 1`, lineID).
		Scan(&remotePort)

	var nodeIP string
	var nodeID int64
	// 优先用 public_ip（给玩家看的那个），没有才退回 ip（运维连的那条）。
	// 二者经常不是同一个值：节点常见配置是 ip=127.0.0.1、public_ip=真实公网地址。
	if err := s.db.QueryRow(`SELECT id, COALESCE(NULLIF(public_ip,''), ip, '') FROM nodes ORDER BY id LIMIT 1`).
		Scan(&nodeID, &nodeIP); err != nil {
		writeErr(w, http.StatusBadRequest, "还没有登记任何节点，无法自检")
		return
	}
	// ⚠️ 没有绑定端口时**不要拨**：以前会拿 BindPort=0 下去，daemon 兜底成 7000，
	// 于是报「dial …:7000 connection refused」—— 那是个根本不存在的目标，
	// 用户会以为是线路/防火墙问题（2026-10-24 用户实测就是这个现象）。
	if remotePort <= 0 {
		hints := []string{"这条线路下还没有绑定任何端口，玩家没有可连的入口（先在「线路分配」里绑一条隧道/端口）"}
		hints = append(hints, directAddrHints(host, nodeIP)...)
		hints = append(hints, "提示：绑定端口后再自检，才会真正去验证端口是否对外可达")
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "dial_ok": false, "register_ok": false,
			"error": "公网直出自检未通过：这条线路还没有绑定任何端口",
			"hints": hints, "mode": "direct", "skipped": true,
		})
		return
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "节点连接失败："+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	resp, err := cli.TestFrps(ctx, &pb.TestFrpsRequest{Host: host, BindPort: remotePort, DialOnly: true})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "自检请求失败："+err.Error())
		return
	}

	hints := []string{}
	if remotePort <= 0 {
		hints = append(hints, "该线路下还没有绑定任何端口，玩家没有可连的入口")
	}
	hints = append(hints, directAddrHints(host, nodeIP)...)
	if remotePort > 0 && !resp.DialOk {
		hints = append(hints, fmt.Sprintf("从节点拨 %s:%d 不通：%s", host, remotePort, resp.DialError))
		hints = append(hints, "可能原因：① 端口没对外（实例没跑 / 容器没发布该端口）；② 宿主机防火墙未放行；③ 从节点拨自己的公网 IP 会遇到 NAT 回环（部分云主机不支持），此时请从**外部**验证")
	}

	success := remotePort > 0 && resp.DialOk
	msg := fmt.Sprintf("公网直出自检通过：节点可达 %s:%d", host, remotePort)
	if !success {
		msg = "公网直出自检未通过"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": success, "dial_ok": resp.DialOk, "dial_ms": resp.DialMs,
		"dial_error": resp.DialError, "register_ok": false,
		"address":  fmt.Sprintf("%s:%d", host, remotePort),
		"error":    msg, "hints": hints, "mode": "direct",
	})
}

// directAddrHints 公网直出的「地址」判据。
//
// 只在节点登记了**真实公网 IP** 时才做一致性比对：
// 很多节点登记的是 127.0.0.1 或内网地址（本来就没配），拿它比对只会制造噪音
// —— 用户看到「线路地址与节点 IP 不一致」却无从下手。这种情况改为明确提示先登记。
func directAddrHints(host, nodeIP string) []string {
	if nodeIP == "" || !isPublicIPv4(nodeIP) {
		return []string{fmt.Sprintf("节点登记的公网 IP 是 %q（看起来不是公网地址），因此无法据此核对线路地址；如需校验请先把节点的公网 IP 补全", nodeIP)}
	}
	if host != nodeIP {
		return []string{fmt.Sprintf("线路地址 %s 与节点公网 IP %s 不一致 —— 玩家连的是线路地址，写错就连不上", host, nodeIP)}
	}
	return nil
}

// isPublicIPv4 判断是否是「可用于对外比对」的公网 IPv4。
func isPublicIPv4(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	if v4[0] == 10 || v4[0] == 127 || v4[0] == 0 {
		return false
	}
	if v4[0] == 192 && v4[1] == 168 {
		return false
	}
	if v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31 {
		return false
	}
	if v4[0] == 169 && v4[1] == 254 {
		return false
	}
	return true
}
